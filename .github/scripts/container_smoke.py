#!/usr/bin/env python3
"""Exercise a release image's MCP entrypoint with an isolated ephemeral store."""

import argparse
import json
from pathlib import Path
import queue
import re
import shutil
import subprocess
import sys
import threading
import time
import uuid


TOOLS = {
    "memory_search", "memory_search_batch", "memory_add", "memory_alias",
    "memory_reflect", "checkpoint_save", "checkpoint_resume",
}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def dockerfile_version(path):
    source = Path(path).read_text(encoding="utf-8-sig")
    downloads = re.findall(
        r"https://github\.com/angelnicolasc/graymatter/releases/download/"
        r"v([^/\s]+)/graymatter_([^_\s]+)_linux_\$\{TARGETARCH\}\.tar\.gz",
        source,
    )
    require(len(downloads) == 1, "Dockerfile must select exactly one versioned Linux archive")
    tag, asset = downloads[0]
    require(tag == asset, f"Dockerfile release tag {tag} differs from archive version {asset}")
    return tag


def decode(line):
    response = json.loads(line)
    require(isinstance(response, dict) and response.get("jsonrpc") == "2.0",
            f"Invalid JSON-RPC stdout: {response!r}")
    return response


def notification(response):
    return "id" not in response and isinstance(response.get("method"), str) and "result" not in response and "error" not in response


def smoke(image, platform, mode, version):
    require(shutil.which("docker"), "Docker is required to execute the container smoke")
    cli = subprocess.check_output([
        "docker", "run", "--rm", "--platform", platform, "--entrypoint",
        "graymatter", image, "--version",
    ], text=True, encoding="utf-8", timeout=60).strip()
    require(cli and cli.split()[-1].lstrip("v") == version.lstrip("v"),
            f"CLI version mismatch: {cli!r}, expected {version}")
    name = "graymatter-smoke-" + uuid.uuid4().hex
    command = [
        "docker", "run", "--rm", "-i", "--name", name, "--platform", platform,
        "--env", "GRAYMATTER_OLLAMA_URL=disabled://container-smoke",
        "--env", "GRAYMATTER_KG=0", image,
        "--quiet", "--dir", "/tmp/container-smoke",
    ]
    if mode == "direct":
        command.append("--no-daemon")
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True, encoding="utf-8")
    responses = queue.Queue()
    stderr = []
    records = []
    notifications = []
    reader_errors = []

    def read_stdout():
        try:
            for line in process.stdout:
                responses.put(line)
        except Exception as exc:
            responses.put(exc)
        finally:
            responses.put(None)

    def read_stderr():
        try:
            stderr.extend(process.stderr.readlines())
        except Exception as exc:
            reader_errors.append(exc)

    readers = [threading.Thread(target=read_stdout, daemon=True),
               threading.Thread(target=read_stderr, daemon=True)]
    for reader in readers:
        reader.start()

    def request(method, params):
        seq = len(records) + 1
        process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": seq, "method": method, "params": params}) + "\n")
        process.stdin.flush()
        deadline = time.monotonic() + 30
        while True:
            remaining = deadline - time.monotonic()
            require(remaining > 0, f"Timed out awaiting {method}")
            try:
                line = responses.get(timeout=remaining)
            except queue.Empty as exc:
                raise RuntimeError(f"Timed out awaiting {method}") from exc
            require(line is not None, f"Unexpected stdout EOF while awaiting {method}")
            if isinstance(line, Exception):
                raise RuntimeError("Failed to read container stdout") from line
            response = decode(line)
            if notification(response):
                notifications.append(response)
                continue
            require(response.get("id") == seq, f"Unexpected response ID: {response!r}")
            require("error" not in response and isinstance(response.get("result"), dict),
                    f"MCP request failed: {response!r}")
            require(not response["result"].get("isError", False), f"MCP tool failed: {response!r}")
            records.append({"method": method, "params": params, "response": response})
            return response["result"]

    def call(tool, **arguments):
        result = request("tools/call", {"name": tool, "arguments": {"agent_id": "container-smoke", **arguments}})
        structured = result.get("structuredContent")
        require(isinstance(structured, dict), f"Missing structured result from {tool}: {result!r}")
        return structured

    try:
        info = request("initialize", {
            "protocolVersion": "2024-11-05", "capabilities": {},
            "clientInfo": {"name": "container-smoke", "version": "1"},
        })
        require(info.get("protocolVersion") == "2024-11-05", f"Unexpected negotiated protocol: {info!r}")
        require(info.get("serverInfo", {}).get("version", "").lstrip("v") == version.lstrip("v"),
                f"MCP version differs from CLI: {info!r}")
        require(isinstance(info.get("instructions"), str) and "__shared__" in info["instructions"],
                "Initialize must publish current shared-memory instructions")
        process.stdin.write('{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
        process.stdin.flush()
        tools = request("tools/list", {}).get("tools", [])
        require(len(tools) == len(TOOLS) and {tool.get("name") for tool in tools} == TOOLS,
                f"Unexpected tool inventory: {tools!r}")
        state = {"task": "container-smoke", "phase": "verified"}
        saved = call("checkpoint_save", state=json.dumps(state))
        resumed = call("checkpoint_resume")
        require(saved.get("checkpoint_id") and resumed.get("id") == saved["checkpoint_id"]
                and resumed.get("state") == state, f"Checkpoint did not round-trip: {resumed!r}")
        old = "Container smoke service uses port 48120."
        new = "Container smoke service uses port 48121."
        require(call("memory_add", text=old).get("stored") is True, "Memory was not stored")
        alias = call("memory_alias", term="service", equivalents=["application"])
        require(alias.get("stored") is True and alias.get("term") == "service"
                and alias.get("equivalents") == ["application"], f"Alias was not stored: {alias!r}")
        require(old in (call("memory_search", query="Container smoke service port", top_k=8).get("facts") or []),
                "Stored memory missing from search")
        batch = call("memory_search_batch", queries=["Container smoke service port", "application"], top_k=8)
        require(old in (batch.get("merged") or []) and len(batch.get("per_query", [])) == 2
                and all(old in (item.get("facts") or []) and not item.get("error") for item in batch["per_query"]),
                f"Stored memory missing from batch/alias search: {batch!r}")
        for action in ("pin", "unpin"):
            require(call("memory_reflect", action=action, text=old).get("ok") is True,
                    f"Memory {action} failed")
        require(call("memory_reflect", action="update", target=old, text=new).get("ok") is True,
                "Memory update failed")
        found = call("memory_search", query="Container smoke service port", top_k=8).get("facts") or []
        require(new in found and old not in found, f"Updated memory is stale: {found!r}")
        require(call("memory_reflect", action="forget", text=new).get("ok") is True, "Memory forget failed")
        found = call("memory_search", query="Container smoke service port", top_k=8).get("facts") or []
        require(old not in found and new not in found, f"Forgotten memory is still searchable: {found!r}")
        require(len(records) == 14, f"Incomplete smoke: {len(records)} requests, expected 14")
        process.stdin.close()
        require(process.wait(timeout=20) == 0, f"Container exited with status {process.returncode}")
        for reader in readers:
            reader.join(timeout=5)
            require(not reader.is_alive(), "Container output pipes did not close")
        require(not reader_errors, f"Failed to read container stderr: {reader_errors!r}")
        # Validate every remaining stdout line, including output after the last call.
        while True:
            line = responses.get_nowait()
            if line is None:
                require(responses.empty(), "Stdout data followed EOF")
                break
            if isinstance(line, Exception):
                raise RuntimeError("Failed to read trailing container stdout") from line
            response = decode(line)
            require(notification(response), f"Unexpected trailing stdout response: {response!r}")
            notifications.append(response)
        return {"status": "PASS", "image": image, "platform": platform, "mode": mode,
                "cli_version": cli, "calls": len(records), "records": records,
                "notifications": notifications}
    except Exception:
        print(json.dumps({"status": "FAIL", "image": image, "platform": platform,
                          "mode": mode, "records": records}, ensure_ascii=False, indent=2))
        print("Container stderr:\n" + "".join(stderr), file=sys.stderr)
        raise
    finally:
        # The unique name confines cleanup to the container started above.
        try:
            subprocess.run(["docker", "rm", "--force", name], capture_output=True, timeout=20)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream and not stream.closed:
                    stream.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("platform", choices=["linux/amd64", "linux/arm64"])
    parser.add_argument("mode", choices=["direct", "daemon"])
    parser.add_argument("--dockerfile", default="Dockerfile")
    args = parser.parse_args()
    result = smoke(args.image, args.platform, args.mode, dockerfile_version(args.dockerfile))
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
