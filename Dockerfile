FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /reservation-service ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /reservation-service /reservation-service
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/reservation-service", "-state-file", "/data/state.json"]
