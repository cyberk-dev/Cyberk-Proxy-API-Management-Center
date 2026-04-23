FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

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
