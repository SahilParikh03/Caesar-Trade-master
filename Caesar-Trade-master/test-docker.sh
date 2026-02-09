#!/usr/bin/env bash
set -euo pipefail

# Docker Integration Test Runner
# Verifies signer's zero-disk (memguard/mlock) and UDS logic in containers.

COMPOSE="docker compose"
TEST_EXIT=0

cleanup() {
    echo ""
    echo "==> Tearing down containers..."
    $COMPOSE --profile test down --volumes --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Starting localstack and signer..."
$COMPOSE up -d localstack signer

echo "==> Waiting for LocalStack to be ready..."
for i in $(seq 1 30); do
    if docker compose exec -T localstack curl -sf http://localhost:4566/_localstack/health > /dev/null 2>&1; then
        echo "    LocalStack is healthy."
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: LocalStack failed to become healthy after 30s."
        exit 1
    fi
    sleep 1
done

echo "==> Waiting for Signer container to be running..."
for i in $(seq 1 15); do
    if [ "$(docker compose ps signer --format '{{.State}}' 2>/dev/null)" = "running" ]; then
        echo "    Signer is running."
        break
    fi
    if [ "$i" -eq 15 ]; then
        echo "ERROR: Signer failed to start after 15s."
        echo "    Signer logs:"
        $COMPOSE logs signer
        exit 1
    fi
    sleep 1
done

echo "==> Running integration tests in test-runner container..."
echo ""
if $COMPOSE run --rm test-runner; then
    TEST_EXIT=0
else
    TEST_EXIT=$?
fi

echo ""
if [ "$TEST_EXIT" -eq 0 ]; then
    echo "========================================="
    echo "  PASS: Docker integration tests passed"
    echo "========================================="
else
    echo "========================================="
    echo "  FAIL: Docker integration tests failed"
    echo "========================================="
fi

exit $TEST_EXIT
