#!/bin/sh
# Install the secret-blocking pre-commit hook. Hooks don't travel with clones.
set -e
cd "$(git rev-parse --show-toplevel)"
cp scripts/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit
echo "installed pre-commit hook"
