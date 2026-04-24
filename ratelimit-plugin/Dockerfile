FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Auto-bump upstream CLIProxyAPI SDK to latest when BUMP_SDK=true.
# BUMP_TS (unix timestamp) is passed by cyberk-worker.sh every run to bust
# Docker's layer cache so `go get` re-checks proxy.golang.org each deploy.
# Without BUMP_SDK=true, the pinned version in go.mod is used (default).
ARG BUMP_SDK=false
ARG BUMP_TS=0
RUN if [ "$BUMP_SDK" = "true" ]; then \
      echo "bump SDK at ts=${BUMP_TS}" && \
      go get github.com/router-for-me/CLIProxyAPI/v6@latest && \
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
