package srv

import (
	"io/ioutil"
	"net/http"

	"github.com/go-chi/render"
)

type response struct {
	Status string      `json:"status"`
	Data   interface{} `json:"data"`
}

func IfError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	Error(w, r, 500, err.Error())
	return true
}

func Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	render.Status(r, code)
	resp := response{Status: "error", Data: msg}
	render.JSON(w, r, resp)
}

func JSON(w http.ResponseWriter, r *http.Request, code int, payload interface{}) {
	render.Status(r, code)
	render.JSON(w, r, response{Status: "ok", Data: payload})
}

func SendFile(w http.ResponseWriter, r *http.Request, path string, contentType string) {
	w.Header().Set("Content-Type", contentType)
	data, err := ioutil.ReadFile(path)
	if IfError(w, r, err) {
		return
	}
	w.Write(data)
}
