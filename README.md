# GitHub Core Clone

GitHub-compatible core services in a single Go process: Git smart HTTP,
REST v3, GraphQL, and MCP. The server keeps repositories, Git objects, refs,
issues, and pull requests in memory and exposes them through compatible API
routes.

## Run

Build the static server with Go 1.24 and run it directly:

```sh
export PATH="$HOME/bin/go/bin:$PATH"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
PORT=8080 ./forge
```

The `PORT` environment variable selects the listening port. If omitted or set
to `0`, the server asks the operating system for an available port. The
provided Node launcher starts the committed `forge` binary with the current
environment:

```sh
PORT=8080 node main.ts
```

## Exercise the Git service

With the server running on port 8080, create an initialized repository and
query or clone it with standard Git:

```sh
curl -fsS -X POST -H 'Authorization: token forge-demo' \
  -H 'Content-Type: application/json' -d '{"name":"demo","auto_init":true}' \
  http://127.0.0.1:8080/api/v3/user/repos
git ls-remote http://127.0.0.1:8080/adaiversen/demo.git
git clone --depth=1 http://127.0.0.1:8080/adaiversen/demo.git demo-shallow
git clone http://127.0.0.1:8080/adaiversen/demo.git demo-full
git -C demo-full config user.name Tester
git -C demo-full config user.email tester@example.test
printf 'push test\\n' > demo-full/push-test.txt
git -C demo-full add push-test.txt
git -C demo-full commit -m 'Test receive-pack'
git -C demo-full push http://127.0.0.1:8080/adaiversen/demo.git HEAD:refs/heads/test
```

The REST API is rooted at `/api/v3`, GraphQL at `/api/graphql`, and MCP JSON-RPC
at `/mcp`. `/healthz` returns a small health response. API and push requests use
the `forge-demo` token by default; set `MAKEFUN_SHOP_TOKEN` to change it.
