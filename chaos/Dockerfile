# Built from the REPOSITORY ROOT, not from chaos/: go.mod replaces the hammer module
# with ../hammer to share txkit, so the build context has to contain both.
#
#   docker build -f chaos/Dockerfile .
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git gcc musl-dev linux-headers
WORKDIR /src
COPY hammer/go.mod hammer/go.sum ./hammer/
COPY chaos/go.mod chaos/go.sum ./chaos/
RUN cd chaos && go mod download
COPY hammer ./hammer
COPY chaos ./chaos
RUN cd chaos && CGO_ENABLED=1 go build -trimpath -o /out/pbtchaos .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pbtchaos /usr/local/bin/pbtchaos
ENTRYPOINT ["pbtchaos"]
