FROM golang:1.24@sha256:764d7e0ce1df1e4a1bddc6d1def5f3516fdc045c5fad88e61f67fdbd1857282f as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN go vet -v ./...
RUN go test -v ./...

RUN CGO_ENABLED=0 go build -o /go/bin/app ./cmd/token-handler

FROM gcr.io/distroless/static@sha256:b7b9a6953e7bed6baaf37329331051d7bdc1b99c885f6dbeb72d75b1baad54f9
USER nonroot:nonroot
COPY --from=build --chown=nonroot:nonroot /go/bin/app /
ENTRYPOINT ["/app"]
