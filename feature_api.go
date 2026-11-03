package main

import (
	"errors"
	"net/http"
)

func (s *Server) groupsSave(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		Mode   string       `json:"mode"`
		Groups []RelayGroup `json:"groups"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	if err := s.workspace.saveGroups(v.Mode, v.Groups); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) groupsDelete(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		ID          string `json:"id"`
		DeleteNodes bool   `json:"deleteNodes"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	if err := s.workspace.deleteGroup(v.ID, v.DeleteNodes); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) jobsStart(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		Kind string   `json:"kind"`
		IDs  []string `json:"ids"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	var id string
	var err error
	switch v.Kind {
	case "refresh":
		id, err = s.workspace.refreshJob(v.IDs)
	case "pool-check":
		id, err = s.workspace.poolCheckJob(v.IDs)
	case "measure":
		id, err = s.workspace.batchMeasureJob()
	default:
		err = errors.New("不支持的后台任务")
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"accepted": true, "jobID": id})
}
func (s *Server) jobsCancel(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	id, err := readID(w, r)
	if err != nil {
		fail(w, err)
		return
	}
	s.workspace.cancelJob(id)
	respond(w, map[string]bool{"ok": true})
}
