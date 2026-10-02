package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHelperProcess(t *testing.T) {
	mode := ""
	readyFile := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "usage-helper=") {
			mode = strings.TrimPrefix(arg, "usage-helper=")
		}
		if strings.HasPrefix(arg, "usage-ready-file=") {
			readyFile = strings.TrimPrefix(arg, "usage-ready-file=")
		}
	}
	if mode == "" {
		return
	}
	if mode == "hold-stdout" {
		// A descendant keeps only the inherited stdout/stdin open. It exits
		// when the adapter closes its input, with a bounded fallback so a
		// failed regression never leaves a test process running indefinitely.
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		os.Exit(0)
	}
	scanner := bufio.NewScanner(os.Stdin)
	expected := []string{"initialize", "initialized", "account/rateLimits/read"}
	for i := 0; i < 3; i++ {
		if !scanner.Scan() {
			os.Exit(4)
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Method != expected[i] {
			os.Exit(5)
		}
		if i == 0 {
			if mode == "descendant-stdout" {
				child := exec.Command(os.Args[0], "-test.run=^TestCodexHelperProcess$", "--", "usage-helper=hold-stdout")
				hideCommandWindow(child)
				child.Stdin, child.Stdout = os.Stdin, os.Stdout
				if err := child.Start(); err != nil {
					os.Exit(7)
				}
				if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
					os.Exit(8)
				}
				os.Exit(0)
			}
			if mode == "timeout" {
				time.Sleep(30 * time.Second)
				os.Exit(6)
			}
			fmt.Printf("{\"id\":%d,\"result\":{\"userAgent\":\"synthetic\"}}\n", req.ID)
		}
		if i == 2 {
			switch mode {
			case "rpc-error":
				fmt.Printf("{\"id\":%d,\"error\":{\"code\":-32000,\"message\":\"secret must not escape\"}}\n", req.ID)
			case "request":
				fmt.Println(`{"id":42,"method":"account/chatgptAuthTokens/refresh","params":{}}`)
			default:
				fmt.Printf("{\"id\":%d,\"result\":{\"rateLimits\":{\"limitId\":\"codex\",\"primary\":{\"usedPercent\":17,\"windowDurationMins\":300,\"resetsAt\":2000000000},\"secondary\":null}}}\n", req.ID)
			}
		}
	}
	os.Exit(0)
}

func TestCodexDeadlineClosesDescendantHeldStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readyFile := filepath.Join(t.TempDir(), "ready")
	c := Connection{ID: "synthetic", Provider: "openai", Kind: "codex-app-server", AccountID: "synthetic", Command: []string{os.Args[0], "-test.run=^TestCodexHelperProcess$", "--", "usage-helper=descendant-stdout", "usage-ready-file=" + readyFile}}
	done := make(chan error, 1)
	go func() { _, err := readCodex(ctx, c, time.Now().UTC()); done <- err }()
	readyDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("fixture did not start descendant")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected canceled wrapper read")
		}
	case <-time.After(time.Second):
		t.Fatal("descendant stdout defeated cancellation")
	}
}

func TestReadOnlyCodexProtocol(t *testing.T) {
	for _, mode := range []string{"success", "rpc-error", "request", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				defer cancel()
			}
			c := Connection{ID: "codex", Provider: "openai", Kind: "codex-app-server", AccountID: "me", Command: []string{os.Args[0], "-test.run=^TestCodexHelperProcess$", "--", "usage-helper=" + mode}}
			s, e := readCodex(ctx, c, time.Now().UTC())
			if mode == "success" {
				if e != nil || len(s.Quotas) != 2 || *s.Quotas[0].UsedPercent != 17 {
					t.Fatalf("protocol: %+v %v", s, e)
				}
			} else {
				if e == nil {
					t.Fatal("failure accepted")
				}
				if strings.Contains(e.Error(), "secret") {
					t.Fatal("provider error payload leaked")
				}
			}
		})
	}
}
