FROM node:20-alpine AS frontend-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25.3-alpine AS server-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=frontend-build /src/frontend/dist ./frontend/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/modelrail .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server-build /out/modelrail /usr/local/bin/modelrail
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/modelrail"]
