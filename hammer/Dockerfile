FROM golang:1.26-alpine AS build
RUN apk add --no-cache git gcc musl-dev linux-headers
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -o /out/pbthammer .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pbthammer /usr/local/bin/pbthammer
ENTRYPOINT ["pbthammer"]
