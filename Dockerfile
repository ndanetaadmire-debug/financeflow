FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/financeflow ./cmd/financeflow

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/financeflow /financeflow
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/financeflow"]
