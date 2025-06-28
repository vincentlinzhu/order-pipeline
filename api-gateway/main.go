package main

import (
  "encoding/json"
  "log"
  "net/http"
)

type Order struct {
  ID  string `json:"id"`
  SKU string `json:"sku"`
  Qty int    `json:"qty"`
}

func handleOrders(w http.ResponseWriter, r *http.Request) {
  if r.Method != http.MethodPost {
    http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    return
  }

  var o Order
  if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
    http.Error(w, "Invalid JSON", http.StatusBadRequest)
    return
  }
  // TODO: add field validation here
  if o.ID == "" || o.SKU == "" || o.Qty <= 0 {
	http.Error(w, "Missing or invalid fields", http.StatusBadRequest)
	return
  }


  w.WriteHeader(http.StatusAccepted)
  w.Write([]byte("Order received: " + o.ID))
}

func main() {
  mux := http.NewServeMux()
  mux.HandleFunc("/orders", handleOrders)

  addr := ":8080"
  log.Println("API Gateway listening on", addr)
  if err := http.ListenAndServe(addr, mux); err != nil {
    log.Fatal(err)
  }
}
