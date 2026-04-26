FROM golang:1.25 AS builder

WORKDIR /magneto.internal

# Get the dependencies so it can be cached into a layer
COPY go.mod go.sum ./
RUN go mod download

# Now copy all the source...
COPY . .

# ...and build it.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ./bin/magneto \
    -ldflags="-s -w" \
    .

FROM alpine:3.23

ARG UID=10001
ARG GID=10001
ARG USER_NAME=appuser
ARG GROUP_NAME=appgroup
ARG WORKSPACE=/app

WORKDIR ${WORKSPACE}

ENV PATH="${PATH}:${WORKSPACE}/bin"

RUN apk --no-cache add ca-certificates
RUN addgroup -g $GID -S $GROUP_NAME \
    && adduser --shell /sbin/nologin --disabled-password \
    -h "${WORKSPACE}" \
    --no-create-home -u $UID -S $USER_NAME -G $GROUP_NAME


COPY --from=builder /magneto.internal/bin/magneto ./bin/magneto
USER "${USER_NAME}"

# Command to run the executable
ENTRYPOINT ["magneto"]
