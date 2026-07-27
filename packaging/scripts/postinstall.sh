#!/bin/sh
# Post-install script for Rysh .deb and .rpm packages

set -e

# Create config directory if it does not exist
if [ ! -d /etc/rysh ]; then
    mkdir -p /etc/rysh
fi

# Create user config stub if not present
CONFIG_DIR="${HOME:-/root}/.config/rysh"
if [ ! -d "$CONFIG_DIR" ]; then
    mkdir -p "$CONFIG_DIR"
fi

# Create state directory
STATE_DIR="${HOME:-/root}/.local/state/rysh"
if [ ! -d "$STATE_DIR" ]; then
    mkdir -p "$STATE_DIR"
fi

echo ""
echo "Rysh installed successfully."
echo ""
echo "Quick start:"
echo "  rysh                         # start the default session"
echo "  rysh my-project              # start a named session"
echo ""
echo "Configuration:"
echo "  ~/.config/rysh/rysh.config   # user config (TOML)"
echo "  /etc/rysh/rysh.config.example  # example config"
echo ""
echo "Documentation:"
echo "  https://rysh.ai/docs"
echo ""
