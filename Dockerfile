# One build for every command in this repo:
#
#   docker build --build-arg CMD=pbtmonitor -t pbt-monitor:local .
#
# The context is the repository root because one module holds all four commands and the
# packages they share under internal/. .dockerignore is what keeps that context small.
FROM golang:1.26-alpine AS build
ARG CMD
RUN apk add --no-cache gcc musl-dev linux-headers
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO stays on: go-ethereum's BLS support is cgo, and a pure-Go build of it does not exist.
RUN CGO_ENABLED=1 go build -trimpath -o /out/pbt ./cmd/${CMD}

FROM alpine:latest
RUN apk add --no-cache ca-certificates
# The binary is installed under one fixed name because ENTRYPOINT cannot expand a build arg.
# Which command this image actually runs is the image tag, and the Kurtosis service name.
COPY --from=build /out/pbt /usr/local/bin/pbt
ENTRYPOINT ["pbt"]
