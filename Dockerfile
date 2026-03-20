FROM golang:1.23
WORKDIR /app
COPY . .
RUN go build -o starhane-fm-server cmd/main.go
EXPOSE 8000
CMD ["./starhane-fm-server"]
