#!/bin/bash

# Matrimail Setup Script
set -euo pipefail

echo "📨 Matrimail Setup"
echo "========================="

# Build the bridge. No system crypto library to install first: the build uses
# the goolm tag, a pure-Go Olm implementation.
echo "🔨 Building Matrimail..."
make build

if [ ! -f matrimail ]; then
    echo "❌ Build failed"
    exit 1
fi

echo "✅ Build successful!"

# Check if bbctl is available
if ! command -v bbctl &> /dev/null; then
    echo "❌ bbctl not found. Please install Beeper bridge manager."
    echo "   Visit: https://github.com/beeper/bridge-manager"
    exit 1
fi

echo "✅ bbctl found"

# Generate config skeleton via Bridge Manager (bbctl)
mkdir -p ./data

# Ask for a bridge name (required by bbctl). Use a safe default if empty.
read -r -p "Enter a bridge name to use with bbctl (e.g., sh-matrimail-local): " BRIDGE_NAME
BRIDGE_NAME=${BRIDGE_NAME:-sh-matrimail-local}

echo "📝 Generating bridgev2 config for ${BRIDGE_NAME} to ./data/config.yaml..."
# Use the generic bridgev2 template with a custom name. Do not rely on a per-bridge template.
bbctl config --type bridgev2 --output ./data/config.yaml "$BRIDGE_NAME"

cat <<'EONOTE'

🎉 Setup complete!

Next steps:
1) Edit ./data/config.yaml if needed.
   - The generated config uses Beeper websocket mode and includes double puppeting defaults.
   - SQLite DB path defaults to ./data/ relative to where you run the binary.
2) Start the bridge:
   ./matrimail --config ./data/config.yaml

Notes:
- In Beeper websocket mode, a registration.yaml is NOT required.
- If you plan to run against a standard homeserver via HTTP appservice,
  generate a registration with:
    ./matrimail --generate-registration
  and add it to your homeserver config.
EONOTE

echo "Documentation: https://github.com/Leicas/matrimail"
