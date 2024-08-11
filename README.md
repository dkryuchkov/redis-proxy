
# Redis Proxy for Containers

## Overview

This Go package provides a Redis proxy server that isolates multiple clients connecting to a single Redis instance. Each client is authenticated with unique credentials and assigned to an individual Redis database index, preventing them from switching to different indices (i.e., the `SELECT` command is restricted).

Designed to run in a container environment, this proxy uses environment variables and service accounts to configure and manage clients. It automatically scans configured namespaces to build up list of enrolled clients with their credentials and allocated indices.

## License

This project is available under a dual-license model:

1. **MIT License**:  
   - This license allows for free use, modification, and distribution of the software under the terms of the MIT License.  
   - The MIT License applies to non-commercial use or any usage with **up to 5 concurrent client connections**.

2. **Commercial License**:  
   - For commercial use cases that exceed 5 concurrent client connections, a separate commercial license is required.  
   - Please contact Dmitry Kryuchkov at **kryuchkov@hotmail.com** to discuss commercial licensing options.

### Summary of Terms

- **MIT License**:  
  Use, copy, modify, and distribute the software freely for up to 5 concurrent client connections. See the `LICENSE` file for the full MIT License terms.

- **Commercial License**:  
  Required for:
  - More than 5 concurrent client connections.
  - Selling, sublicensing, or distributing the software or derivative works commercially.

## Contact Information

For commercial licensing inquiries, please contact:

**Dmitry Kryuchkov**  
**Email:** kryuchkov@hotmail.com

## Disclaimer

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NONINFRINGEMENT. SEE THE MIT LICENSE FOR MORE DETAILS.


## Features

- **Client Isolation**: Each client has its own credentials and is restricted to a specific Redis database index.
- **Dynamic Credential Management**: Automatically scans Kubernetes namespaces and secrets to build and refresh client credentials.
- **Kubernetes Native**: Designed to integrate seamlessly with Kubernetes, using service accounts and environment variables for configuration.
- **Dockerized Deployment**: Includes a Dockerfile to easily build the image and deploy it in Kubernetes.

## How It Works

### Command Interception and Handling

The Redis Proxy intercepts all commands sent through it to the Redis server. It custom handles specific key commands to enforce client isolation and ensure security:

- **AUTH**: The proxy intercepts the `AUTH` command to validate the client's credentials. Instead of directly authenticating the client with Redis, the proxy checks the credentials against the client credentials stored in the Kubernetes secrets. If the credentials are valid and not expired, the proxy authenticates the client with the Redis server using the master password retrieved from a designated Kubernetes secret.
  
- **SELECT**: The proxy intercepts the `SELECT` command to enforce client isolation. When a client attempts to select a Redis database index, the proxy checks if the requested index matches the one allocated to the client. If the index does not match, the proxy prevents the switch, ensuring that the client can only access its assigned database index.

- **Other Commands**: For commands other than `AUTH` and `SELECT`, the proxy forwards them to the Redis server as usual, but only if the client has been successfully authenticated.

### Intercepted Commands

1. **AUTH**: Handles client authentication by validating against Kubernetes-stored credentials.
2. **SELECT**: Ensures the client only accesses their allocated Redis database index.

### Redis Credentials Management

The Redis master password and connection string are fetched from a Kubernetes secret. The secret must contain the following keys:

- `redis-password`: The Redis master password.
- `redis-connection-string`: The Redis connection string, including the database index and password.

These credentials are used by the proxy to authenticate itself to the Redis server.

### Client Credentials

Client credentials are dynamically managed by scanning the Kubernetes namespaces for secrets that match the prefix specified in the environment variable `NS_PREFIX`. The client credentials are stored in the secret specified by the `REDIS_SECRET` environment variable. Each client’s credentials are mapped to their allocated Redis database index, ensuring they can only access their assigned database.

### Robust Command Processing

The proxy processes Redis commands robustly by:
- **Parsing Redis Commands**: The proxy reads and parses incoming commands, including multi-line bulk commands.
- **Dynamic Buffer Management**: It adjusts the buffer size dynamically based on the command size, ensuring efficient handling of large bulk strings.
- **Error Handling**: The proxy includes comprehensive error handling to manage and log any anomalies during command processing or communication with Redis.

## Installation

To use the Redis Proxy, clone this repository and build the project:

```bash
git clone https://github.com/dmitry-kryuchkov/redis-proxy.git
cd redis-proxy
go build -o redis-proxy
```

## Docker

A `Dockerfile` is provided to build a Docker image for the Redis Proxy. To build the Docker image:

```bash
docker build -t redis-proxy:latest .
```

## Kubernetes Deployment

You can deploy the Redis Proxy in a Kubernetes cluster using the following example YAML configuration. This configuration assumes that the necessary environment variables are set and that the proxy is configured with access to the required Kubernetes namespaces and secrets.

### Example Deployment YAML

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis-proxy
  labels:
    app: redis-proxy
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis-proxy
  template:
    metadata:
      labels:
        app: redis-proxy
    spec:
      containers:
      - name: redis-proxy
        image: redis-proxy:latest
        ports:
        - containerPort: 6379
        env:
        - name: BASE_WORKERS
          value: "1"
        - name: MAX_WORKERS
          value: "10000"
        - name: WORK_QUEUE_SIZE
          value: "1000"
        - name: WORKER_IDLE_TIMEOUT_SEC
          value: "30"
        - name: KUBERNETES_PORT
          value: "443"
        - name: REDIS_SECRET
          value: "redis-secret-master"
        - name: NS_PREFIX
          value: "client-"
        volumeMounts:
        - name: kube-config
          mountPath: /root/.kube
          readOnly: true
      volumes:
      - name: kube-config
        hostPath:
          path: /root/.kube
```

## Environment Variables

- `BASE_WORKERS`: Number of initial worker goroutines (default: 1)
- `MAX_WORKERS`: Maximum number of worker goroutines (default: 1000)
- `WORK_QUEUE_SIZE`: Size of the work queue (default: 1000)
- `WORKER_IDLE_TIMEOUT_SEC`: Worker idle timeout in seconds (default: 30)
- `KUBERNETES_PORT`: Kubernetes API server port (default: 443)
- `REDIS_SECRET`: Name of the Kubernetes secret containing Redis credentials.
- `NS_PREFIX`: Prefix for namespaces to be scanned for Redis secrets.

## Usage

To run the Redis Proxy locally:

```bash
./redis-proxy
```

Ensure that the necessary environment variables are set and that the proxy is configured with access to the required Kubernetes namespaces and secrets.

## Testing

This project includes comprehensive unit tests to ensure the correct functionality of the Redis Proxy. The tests cover key areas such as:

- Authentication handling (`AUTH` command)
- Database selection handling (`SELECT` command)
- Command forwarding to Redis
- Credential refreshing from Kubernetes secrets
- Worker management

### Running the Tests

To run the tests, use the following command:

```bash
go test ./...

## License

This project is licensed under the MIT License - see the [LICENSE](./LICENSE) file for details.

## Contributing

Contributions are welcome! Please open an issue or submit a pull request for any bugs or feature requests.

## Authors

- **Dmitry Kryuchkov** - *Initial work* - [Dmitry Kryuchkov GitHub Profile](https://github.com/dkryuchkov)

