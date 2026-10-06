FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_VERSION=0.0.1
ARG BUILD_HASH=unknown
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.BuildVersion=${BUILD_VERSION} -X main.BuildHash=${BUILD_HASH}" -o /server ./cmd/server

# migrations are embedded in the binary and run on boot before the server listens
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENV HTTP_PORT=8080
ENTRYPOINT ["/server"]
