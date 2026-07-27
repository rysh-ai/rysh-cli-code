#!/bin/sh
# Pre-remove script for Rysh .deb and .rpm packages

set -e

# Stop any running rysh sessions gracefully
if command -v rysh >/dev/null 2>&1; then
    # List and attempt to terminate running sessions
    rysh list-sessions 2>/dev/null | grep running | awk '{print $1}' | while read session; do
        echo "Stopping rysh session: $session"
        rysh detach "$session" 2>/dev/null || true
    done
fi

echo "Rysh removed. Session data is project-local and untouched: each project"
echo "keeps its own .rysh directory next to its rysh.config.yaml."
echo "To remove it for a project: rm -rf <project>/.rysh"
