#!/bin/sh
# Post-install script for Rysh .deb and .rpm packages

set -e

# Create config directory if it does not exist
if [ ! -d /etc/rysh ]; then
    mkdir -p /etc/rysh
fi

# No user config or state directory is created: rysh reads neither. Config and
# state are project-local — ./rysh.config.yaml or ./.rysh/rysh.config.yaml, with
# state under ./.rysh — so there is nothing to seed in $HOME.

echo ""
echo "Rysh installed successfully."
echo ""
echo "Quick start:"
echo "  rysh                         # start the default session"
echo "  rysh my-project              # start a named session"
echo ""
echo "Configuration (project-local — no global config file):"
echo "  ./rysh.config.yaml                    # per-project config"
echo "  /etc/rysh/rysh.config.yaml.example    # copy this to start"
echo ""
echo "Documentation:"
echo "  https://rysh.ai/docs"
echo ""
