FROM golang:1.17 as builder

WORKDIR /aws-node-asg-detach
COPY go.mod .
COPY go.sum .

ARG CGO_ENABLED=0
ARG GOOS=linux
ARG GOARCH=amd64

RUN go mod download

# Build
COPY . .
RUN go build

# Build the final image with only the binary
FROM alpine:3

WORKDIR /
COPY --from=builder /aws-node-asg-detach/aws-node-asg-detach .

USER 1000
ENTRYPOINT ["/aws-node-asg-detach"]
