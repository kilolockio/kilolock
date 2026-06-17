# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build
WORKDIR /src

ARG KL_GO_TAGS=""

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="-s -w" -o /out/kl ./cmd/kl
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="-s -w" -o /out/kld ./cmd/kld
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="-s -w" -o /out/klc ./cmd/klc

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /

COPY --from=build /out/kl /usr/local/bin/kl
COPY --from=build /out/kld /usr/local/bin/kld
COPY --from=build /out/klc /usr/local/bin/klc

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kld"]
CMD ["serve"]
