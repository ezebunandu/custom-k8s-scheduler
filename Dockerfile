FROM golang:1.23 AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /scheduler ./cmd/scheduler

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /scheduler /scheduler
ENTRYPOINT ["/scheduler"]
