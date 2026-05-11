FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Auto-bump upstream CLIProxyAPI SDK to latest. BUMP_TS (unix timestamp) is
# passed by cyberk-worker.sh every run to bust Docker's layer cache so
# `go get` re-checks proxy.golang.org each deploy. Default is true so manual
# `docker build .` (no flags) also tracks latest — go.mod's pinned version
# acts as a floor, not the runtime version. Pass --build-arg BUMP_SDK=false
# to opt out and use go.mod verbatim (e.g. for reproducing a past build).
ARG BUMP_SDK=true
ARG BUMP_TS=0
RUN if [ "$BUMP_SDK" = "true" ]; then \
      echo "bump SDK at ts=${BUMP_TS}" && \
      go get github.com/router-for-me/CLIProxyAPI/v7@latest && \
      go mod tidy; \
    fi

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" \
    -o ./CLIProxyAPI .

FROM alpine:3.22.0

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Ho_Chi_Minh

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

# Same entrypoint as upstream — state file auto-lives at /CLIProxyAPI/ratelimit-state.json
# (same directory as config.yaml).
CMD ["./CLIProxyAPI", "-config", "/CLIProxyAPI/config.yaml"]
