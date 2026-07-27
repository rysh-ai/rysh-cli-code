package web

// File-browser HTTP endpoints for the embedded web UI (mobile drill-down's
// file viewer; usable by the desktop UI too). Thin adapters over the browser
// function injected via SetFSBrowser — the real implementation (pane-cwd
// sandbox, text/image classification, size caps) lives in the actors package
// next to the share relay's .fs responder, so the two surfaces cannot drift.
//
// Reply shapes mirror the relay exactly: a success is the fsListReply /
// fsReadReply JSON (ok:true, snake_case fields), an error is
// {ok:false, error:<code>, message:<detail>} with HTTP 200 — clients switch
// on `ok`, not the status code, matching the rysh-mobile client.

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// SetFSBrowser injects the file-browse implementation. Call before Start.
func (s *Server) SetFSBrowser(fn func(op, paneID, path string, offset, length int64) (any, string, string)) {
	s.fsBrowse = fn
}

func (s *Server) fsError(c *gin.Context, code, message string) {
	c.JSON(http.StatusOK, gin.H{"ok": false, "error": code, "message": message})
}

// handleFSList serves GET /fs/list?pane=<id>&path=<rel>.
func (s *Server) handleFSList(c *gin.Context) {
	if s.fsBrowse == nil {
		s.fsError(c, "disabled", "file browsing is not available")
		return
	}
	reply, code, message := s.fsBrowse("list", c.Query("pane"), c.Query("path"), 0, 0)
	if code != "" {
		s.fsError(c, code, message)
		return
	}
	c.JSON(http.StatusOK, reply)
}

// handleFSRead serves GET /fs/read?pane=<id>&path=<rel>&offset=<n>&length=<n>.
func (s *Server) handleFSRead(c *gin.Context) {
	if s.fsBrowse == nil {
		s.fsError(c, "disabled", "file browsing is not available")
		return
	}
	offset, _ := strconv.ParseInt(c.Query("offset"), 10, 64)
	length, _ := strconv.ParseInt(c.Query("length"), 10, 64)
	reply, code, message := s.fsBrowse("read", c.Query("pane"), c.Query("path"), offset, length)
	if code != "" {
		s.fsError(c, code, message)
		return
	}
	c.JSON(http.StatusOK, reply)
}
