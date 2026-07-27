Name:           rysh
Version:        0.1.0
Release:        1%{?dist}
Summary:        Agentic terminal multiplexer for code development
Group:          Applications/System
License:        MIT
URL:            https://rysh.ai
Source0:        https://packages.rysh.ai/releases/v%{version}/rysh_linux_%{_arch}.tar.gz

BuildArch:      x86_64 aarch64
BuildRequires:  systemd-rpm-macros

%description
Rysh is an agentic terminal multiplexer inspired by Zellij, optimized for
agentic software development workflows. Each pane is an autonomous agent
workspace that can execute shell commands and answer LLM prompts.

Key features:
- Tabs and panes with flexible column layouts
- Per-pane AI agent with 35+ agentic tools
- Interactive terminal support (vim, htop, less, nano)
- Cross-pane coordination and shared output
- Remote collaboration via rysh.ai upstream server

%prep
%setup -q -n rysh_%{version}_linux_%{_arch}

%install
install -D -m 0755 rysh %{buildroot}%{_bindir}/rysh
install -D -m 0644 rysh.config.example %{buildroot}%{_sysconfdir}/rysh/rysh.config.example

%post
echo ""
echo "Rysh installed successfully."
echo "Quick start:  rysh"
echo "Config:       ~/.config/rysh/rysh.config"
echo "Docs:         https://rysh.ai/docs"
echo ""

%preun
if [ $1 -eq 0 ]; then
    # Uninstall: try to stop running sessions
    if command -v rysh >/dev/null 2>&1; then
        rysh list-sessions 2>/dev/null | grep running | awk '{print $1}' | while read session; do
            rysh detach "$session" 2>/dev/null || true
        done
    fi
fi

%files
%license LICENSE
%doc README.md
%{_bindir}/rysh
%config(noreplace) %{_sysconfdir}/rysh/rysh.config.example

%changelog
* Mon Jan 01 2024 Rysh Works <releases@rysh.ai> - 0.1.0-1
- Initial package release
