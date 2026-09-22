FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /qbit_benchmark .

FROM scratch
COPY --from=build /qbit_benchmark /qbit_benchmark
EXPOSE 6969 6881
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/qbit_benchmark", "healthcheck"]
ENTRYPOINT ["/qbit_benchmark"]
CMD ["serve"]
