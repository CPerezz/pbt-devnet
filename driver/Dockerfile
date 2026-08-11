# The driver pins go-ethereum to the exact pbt PR head via a replace directive in
# go.mod, so this build is self-contained: no local checkout in the build context.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git gcc musl-dev linux-headers
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -o /out/pbtdriver .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pbtdriver /usr/local/bin/pbtdriver
ENTRYPOINT ["pbtdriver"]
