# build
FROM golang:1.22 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./main.go

# run
FROM gcr.io/distroless/base-debian12
WORKDIR /
COPY --from=build /app/server /server
ENV PORT=8080
EXPOSE 8080
CMD ["/server"]

