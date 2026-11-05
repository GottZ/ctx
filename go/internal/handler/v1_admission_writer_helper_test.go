package handler

import "net/http"

// admissionWrite is how a route calls the admission writer: with the
// request's own context (r.Context(), never one the route derived with a
// deadline of its own).
func admissionWrite(r *v1Rig, req *http.Request, w http.ResponseWriter, err error) string {
	return r.adm.WriteAdmissionError(req.Context(), w, err)
}
