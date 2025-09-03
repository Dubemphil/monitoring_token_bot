# ---------- Build Stage ----------
FROM golang:1.23-alpine AS builder

# Install git and certs
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go mod files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the app
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# ---------- Final Stage ----------
FROM alpine:latest

# Install CA certs
RUN apk --no-cache add ca-certificates

# Set working directory
WORKDIR /app

# Copy compiled binary
COPY --from=builder /app/main .

# Create logs directory (optional, since it will be mounted anyway)
RUN mkdir -p /app/logs

# Expose port (if needed)
EXPOSE 8080

# Run the app
CMD ["./main"]
