# Flowtest Worker

This worker acts as a bridge between your internal infrastructure and [Flowtest.io](https://flowtest.io). It allows you to run tests against your internal services that are not directly exposed to the internet.

## How it works

1.  **Deploy**: You run this worker within your internal infrastructure (e.g., inside your VPC or on a private server).
2.  **Connect**: The worker connects outbound to Flowtest.io and registers itself to a specific Suite.
3.  **Available**: Once connected, the worker appears in the **Suite Settings** on Flowtest.io.
4.  **Proxy**: When you select this worker for a suite, Flowtest.io will route test requests through this worker to reach your internal services.

## Configuration

The worker is configured using command-line flags.

| Flag | Description | Default | Required |
| :--- | :--- | :--- | :--- |
| `-suite` | The unique identifier of the Suite to join. You can find this in your Flowtest.io Suite Settings. | - | **Yes** |
| `-challenge` | The challenge string for authentication. You can find this in your Flowtest.io Suite Settings. | - | **Yes** |
| `-server` | The Flowtest server base URL. | `https://flowtest.io` | No |
| `-name` | A human-friendly name for this worker to identify it in the UI. | `new-worker` | No |
| `-timeout` | HTTP timeout for proxied requests. | `30s` | No |

## Usage Example

```bash
./flowtest-worker \
  -suite "your-suite-id-here" \
  -challenge "your-challenge-string-here" \
  -name "internal-staging-worker"
```