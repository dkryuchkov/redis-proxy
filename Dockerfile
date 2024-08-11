# # Use the official Golang image to create a build artifact.
FROM golang:1.22.1 as builder

# # Set the working directory inside the container
WORKDIR /app

# # Copy go mod and sum files along with the vendor directory
COPY go.mod go.sum ./

# # Copy the source code
COPY *.go ./

RUN go mod tidy

# # Build the Go app using the vendor directory
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

# # Use a minimal alpine image for the final build
FROM alpine:3.14 

# Set the working directory inside the container
WORKDIR /root/

# Copy the pre-built binary file from the builder stage
COPY --from=builder /app/main .

# Expose port 6379
EXPOSE 6379

# Run the executable
CMD ["./main"]
