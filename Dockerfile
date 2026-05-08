FROM golang:1.23-alpine AS builder
WORKDIR /workspace
COPY go.mod ./
# go.sum may not exist on first build; go mod tidy generates it.
# Once generated, commit go.sum and this layer will cache properly.
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
