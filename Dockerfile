# The worklist alone: a static binary on Alpine (the health check runs through /bin/sh and wget).
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /infra-workflow .

FROM alpine:3.24.2
COPY --from=build /infra-workflow /infra-workflow
EXPOSE 8201 9201
ENTRYPOINT ["/infra-workflow"]
