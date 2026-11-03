// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package conpty

// Console is a pseudo-console with a shell attached. Off Windows it cannot be
// created; the methods exist so callers compile one way on every OS.
type Console struct{}

// Start always fails off Windows.
func Start(argv []string, dir string, env []string, size Size) (*Console, error) {
	return nil, ErrUnsupported
}

func (c *Console) Read([]byte) (int, error)  { return 0, ErrUnsupported }
func (c *Console) Write([]byte) (int, error) { return 0, ErrUnsupported }
func (c *Console) Resize(Size) error         { return ErrUnsupported }
func (c *Console) Pid() int                  { return -1 }
func (c *Console) Wait() (uint32, error)     { return 0, ErrUnsupported }
func (c *Console) Kill() error               { return ErrUnsupported }
func (c *Console) Close() error              { return nil }
