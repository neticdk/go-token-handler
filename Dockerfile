FROM golang:1.21 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN go vet -v ./...
RUN go test -v ./...

RUN CGO_ENABLED=0 go build -o /go/bin/app ./cmd/token-handler

FROM gcr.io/distroless/static
USER nonroot:nonroot
COPY --from=build --chown=nonroot:nonroot /go/bin/app /
ENTRYPOINT ["/app"]
