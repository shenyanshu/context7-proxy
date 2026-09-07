// 管理 API：key 池增删启停与运行总览，契约与 web/src/types.ts 逐字段对齐。
// 空 slice 必须输出 []：DTO 构造处显式初始化，杜绝 Go nil slice 编码成 null。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"context7-proxy/internal/store"
)

// overviewDTO 是 /api/admin/overview 的显式 DTO：不复用嵌入 Stats，
// 用 camelCase tag 与前端 Overview 接口对齐。
type overviewDTO struct {
	TotalRequests  int64                 `json:"totalRequests"`
	CacheHits      int64                 `json:"cacheHits"`
	CacheHitRate   float64               `json:"cacheHitRate"`
	RequestsToday  int64                 `json:"requestsToday"`
	Keys           []store.KeyStatus     `json:"keys"`
	RecentRequests []store.RequestRecord `json:"recentRequests"`
}

// keyListDTO 是 /api/admin/keys 的列表契约。
type keyListDTO struct {
	Keys []store.KeyStatus `json:"keys"`
}

// addedKeyDTO 是 POST /api/admin/keys 的响应契约（自用场景明文回显）。
type addedKeyDTO struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
}

// okDTO 是启停/删除的统一响应契约。
type okDTO struct {
	OK bool `json:"ok"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read stats failed")
		return
	}
	keys, err := s.store.ListKeys()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list keys failed")
		return
	}
	recent, err := s.store.RecentRequests()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read recent requests failed")
		return
	}
	// 显式初始化空 slice：nil 编码为 null，前端遍历会炸
	if keys == nil {
		keys = []store.KeyStatus{}
	}
	if recent == nil {
		recent = []store.RequestRecord{}
	}
	writeJSON(w, http.StatusOK, overviewDTO{
		TotalRequests: stats.TotalRequests, CacheHits: stats.CacheHits,
		CacheHitRate: hitRate(stats), RequestsToday: stats.RequestsToday,
		Keys: keys, RecentRequests: recent,
	})
}

// hitRate 计算命中率：total=0 时返回 0（无请求的除零是合法零值场景）。
func hitRate(st store.Stats) float64 {
	if st.TotalRequests == 0 {
		return 0
	}
	return float64(st.CacheHits) / float64(st.TotalRequests)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list keys failed")
		return
	}
	if keys == nil {
		keys = []store.KeyStatus{}
	}
	writeJSON(w, http.StatusOK, keyListDTO{Keys: keys})
}

// handleAddKey 新增 key：body {"value":"ctx7sk-..."}。
// 无效格式 400、重复 409、成功 201 返回 id+value（自用场景明文回显）。
func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	added, err := s.store.AddKey(body.Value)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, addedKeyDTO{ID: added.ID, Value: added.Value})
	case errors.Is(err, store.ErrInvalidKeyFormat):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrDuplicateKey):
		writeJSONError(w, http.StatusConflict, "duplicate key")
	default:
		writeJSONError(w, http.StatusInternalServerError, "add key failed")
	}
}

// handleSetKeyEnabled 启停指定 key：body {"enabled":bool}。
func (s *Server) handleSetKeyEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)).Decode(&body); err != nil || body.Enabled == nil {
		writeJSONError(w, http.StatusBadRequest, "body must be {\"enabled\":bool}")
		return
	}
	err := s.store.SetKeyEnabled(id, *body.Enabled)
	if errors.Is(err, store.ErrKeyNotFound) {
		writeJSONError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "update key failed")
		return
	}
	writeJSON(w, http.StatusOK, okDTO{OK: true})
}

// handleDeleteKey 删除指定 key。
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteKey(id)
	if errors.Is(err, store.ErrKeyNotFound) {
		writeJSONError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "delete key failed")
		return
	}
	writeJSON(w, http.StatusOK, okDTO{OK: true})
}

// pathID 解析路径通配符 {id}：非数字 400，调用方据此短路。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeJSONError(w, http.StatusBadRequest, "invalid key id")
		return 0, false
	}
	return id, true
}

// maxAdminBodyBytes 管理请求体上限：契约只有 {"value":"..."} /
// {"enabled":bool}，几 KB 足够，防异常客户端拖垮内存。
const maxAdminBodyBytes = 8 << 10

// writeJSON 输出 200/201 的 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode response failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
