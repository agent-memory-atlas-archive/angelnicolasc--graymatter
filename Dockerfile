FROM alpine:3.20
ARG TARGETARCH
RUN apk add --no-cache curl tar ca-certificates && \
    curl -fsSL https://github.com/angelnicolasc/graymatter/releases/download/v0.20.0/graymatter_0.20.0_linux_${TARGETARCH}.tar.gz -o /tmp/graymatter.tar.gz && \
    tar -xz -f /tmp/graymatter.tar.gz -C /usr/local/bin graymatter && \
    rm /tmp/graymatter.tar.gz && \
    chmod +x /usr/local/bin/graymatter
ENTRYPOINT ["graymatter", "mcp", "serve"]
