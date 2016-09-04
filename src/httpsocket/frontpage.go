package main

import "net/http"

func handleFrontpage(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Hi", 200)
}
