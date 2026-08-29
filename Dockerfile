# One build for every command in this repo:
#
#   docker build --build-arg CMD=pbtmonitor -t pbt-monitor:local .
#
# The context is the repository root because one module holds every command
# and the packages they share under internal/. .dockerignore is what keeps
# that context small.
FROM golang:1.26-alpine AS build
ARG CMD
RUN apk add --no-cache gcc musl-dev linux-headers
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO stays on: go-ethereum's BLS support is cgo, and a pure-Go build of it does not exist.
#
# Every command is built, not just the selected one. They are a few megabytes
# each and sharing one build cache costs nothing, while one of them - the
# migration gate - execs another when it hands over, so an image holding a
# single binary could not do its job.
RUN CGO_ENABLED=1 go build -trimpath -o /out/ ./cmd/...
RUN cp /out/${CMD} /out/pbt

FROM alpine:latest
RUN apk add --no-cache ca-certificates
# The selected command is installed under one fixed name because ENTRYPOINT
# cannot expand a build arg; the others keep their own names so a wrapper can
# exec them. Which command an image runs is its tag and its service name.
COPY --from=build /out/ /usr/local/bin/
ENTRYPOINT ["pbt"]
