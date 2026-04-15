FROM golang:1.26@sha256:5f3787b7f902c07c7ec4f3aa91a301a3eda8133aa32661a3b3a3a86ab3a68a36 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN go vet -v ./...
RUN go test -v ./...

RUN CGO_ENABLED=0 go build -o /go/bin/app ./cmd/token-handler

FROM gcr.io/distroless/static@sha256:47b2d72ff90843eb8a768b5c2f89b40741843b639d065b9b937b07cd59b479c6
USER nonroot:nonroot
COPY --from=build --chown=nonroot:nonroot /go/bin/app /
ENTRYPOINT ["/app"]
