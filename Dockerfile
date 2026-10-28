FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
RUN go mod download
RUN mkdir -p /app
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /app/document-review .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=build /app/document-review /app/document-review
COPY templates ./templates
COPY static ./static
USER app
EXPOSE 8080
CMD ["/app/document-review"]
