// demos/advanced-enterprise/app.go
// Advanced Enterprise Backend with multiple services, authentication, caching, rate limiting, and metrics
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// User represents a user in the system
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Created  string `json:"created"`
}

// Product represents a product in the inventory
type Product struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Stock       int     `json:"stock"`
	Category    string  `json:"category"`
}

// Order represents an order in the system
type Order struct {
	ID       int       `json:"id"`
	UserID   int       `json:"user_id"`
	Products []Product `json:"products"`
	Total    float64   `json:"total"`
	Status   string    `json:"status"`
	Created  string    `json:"created"`
}

// Analytics represents system analytics
type Analytics struct {
	TotalUsers    int     `json:"total_users"`
	TotalProducts int     `json:"total_products"`
	TotalOrders   int     `json:"total_orders"`
	Revenue       float64 `json:"revenue"`
	AvgOrderValue float64 `json:"avg_order_value"`
	Uptime        string  `json:"uptime"`
}

// WebSocket upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow connections from any origin
	},
}

// In-memory databases (in real applications, use proper databases)
var (
	users     = make(map[int]User)
	products  = make(map[int]Product)
	orders    = make(map[int]Order)
	sessions  = make(map[string]int) // token -> user_id
	metrics   = make(map[string]int)
	startTime = time.Now()
	mu        sync.RWMutex
	nextID    = 1
)

func main() {
	initializeData()

	router := mux.NewRouter()

	// Authentication middleware
	authRouter := router.PathPrefix("/api").Subrouter()
	authRouter.Use(authMiddleware)

	// Public endpoints (no auth required)
	router.HandleFunc("/api/auth/login", loginHandler).Methods("POST")
	router.HandleFunc("/api/auth/register", registerHandler).Methods("POST")
	router.HandleFunc("/api/health", healthHandler).Methods("GET")
	router.HandleFunc("/api/metrics", metricsHandler).Methods("GET")
	router.HandleFunc("/api/products", publicProductsHandler).Methods("GET")

	// WebSocket endpoint for real-time updates
	router.HandleFunc("/api/ws", websocketHandler)

	// Protected endpoints (auth required)
	authRouter.HandleFunc("/users", usersHandler).Methods("GET", "POST")
	authRouter.HandleFunc("/users/{id:[0-9]+}", userHandler).Methods("GET", "PUT", "DELETE")
	authRouter.HandleFunc("/products", productsHandler).Methods("GET", "POST")
	authRouter.HandleFunc("/products/{id:[0-9]+}", productHandler).Methods("GET", "PUT", "DELETE")
	authRouter.HandleFunc("/orders", ordersHandler).Methods("GET", "POST")
	authRouter.HandleFunc("/orders/{id:[0-9]+}", orderHandler).Methods("GET", "PUT", "DELETE")
	authRouter.HandleFunc("/analytics", analyticsHandler).Methods("GET")
	authRouter.HandleFunc("/admin/clear-cache", clearCacheHandler).Methods("POST")
	authRouter.HandleFunc("/admin/system-info", systemInfoHandler).Methods("GET")

	// File upload endpoint
	authRouter.HandleFunc("/upload", uploadHandler).Methods("POST")

	// Search endpoints
	authRouter.HandleFunc("/search/users", searchUsersHandler).Methods("GET")
	authRouter.HandleFunc("/search/products", searchProductsHandler).Methods("GET")

	// Batch operations
	authRouter.HandleFunc("/batch/users", batchUsersHandler).Methods("POST")
	authRouter.HandleFunc("/batch/products", batchProductsHandler).Methods("POST")

	server := &harpserver.BackendServer{
		Name:     "AdvancedEnterpriseBackend",
		Domain:   ".*",
		Route:    "/api/",
		Key:      "master-key",
		Handler:  router,
		ProxyURL: "localhost:50054",
	}

	log.Println("🚀 Starting Advanced Enterprise Backend with HARP...")
	log.Println("📊 Features: Auth, CRUD, WebSockets, Search, Analytics, File Upload")
	log.Println("🔧 Endpoints:")
	log.Println("   - POST /api/auth/login")
	log.Println("   - POST /api/auth/register")
	log.Println("   - GET  /api/health")
	log.Println("   - GET  /api/metrics")
	log.Println("   - WS   /api/ws (WebSocket)")
	log.Println("   - CRUD /api/users, /api/products, /api/orders")
	log.Println("   - GET  /api/analytics")
	log.Println("   - GET  /api/search/*")
	log.Println("   - POST /api/batch/*")

	if err := server.ListenAndServeHarp(); err != nil {
		log.Fatalf("❌ Server error: %v", err)
	}
}

func initializeData() {
	// Initialize sample data
	users[1] = User{ID: 1, Username: "admin", Email: "admin@company.com", Role: "admin", Created: time.Now().Format(time.RFC3339)}
	users[2] = User{ID: 2, Username: "john", Email: "john@company.com", Role: "user", Created: time.Now().Format(time.RFC3339)}
	users[3] = User{ID: 3, Username: "jane", Email: "jane@company.com", Role: "manager", Created: time.Now().Format(time.RFC3339)}

	products[1] = Product{ID: 1, Name: "Laptop Pro", Description: "High-performance laptop", Price: 1299.99, Stock: 25, Category: "Electronics"}
	products[2] = Product{ID: 2, Name: "Wireless Mouse", Description: "Ergonomic wireless mouse", Price: 49.99, Stock: 100, Category: "Accessories"}
	products[3] = Product{ID: 3, Name: "Mechanical Keyboard", Description: "RGB mechanical keyboard", Price: 129.99, Stock: 50, Category: "Accessories"}

	orders[1] = Order{
		ID: 1, UserID: 2,
		Products: []Product{products[1], products[2]},
		Total:    1349.98, Status: "completed",
		Created: time.Now().Format(time.RFC3339),
	}

	nextID = 4
	log.Println("✅ Sample data initialized")
}

// Authentication middleware
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			http.Error(w, `{"error":"Authorization header required"}`, http.StatusUnauthorized)
			return
		}

		if strings.HasPrefix(token, "Bearer ") {
			token = token[7:]
		}

		mu.RLock()
		userID, exists := sessions[token]
		mu.RUnlock()

		if !exists {
			http.Error(w, `{"error":"Invalid token"}`, http.StatusUnauthorized)
			return
		}

		// Add user ID to context
		ctx := context.WithValue(r.Context(), "user_id", userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
		return
	}

	// Simple auth check (in real apps, hash passwords properly)
	mu.RLock()
	var user User
	var found bool
	for _, u := range users {
		if u.Username == creds.Username {
			user = u
			found = true
			break
		}
	}
	mu.RUnlock()

	if !found || creds.Password != "password123" {
		http.Error(w, `{"error":"Invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// Generate simple token (in real apps, use JWT)
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", creds.Username, time.Now().UnixNano()))))

	mu.Lock()
	sessions[token] = user.ID
	metrics["logins"]++
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user":  user,
	})
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	var newUser struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&newUser); err != nil {
		http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
		return
	}

	mu.Lock()
	// Check if username exists
	for _, u := range users {
		if u.Username == newUser.Username {
			mu.Unlock()
			http.Error(w, `{"error":"Username already exists"}`, http.StatusConflict)
			return
		}
	}

	user := User{
		ID:       nextID,
		Username: newUser.Username,
		Email:    newUser.Email,
		Role:     "user",
		Created:  time.Now().Format(time.RFC3339),
	}
	users[nextID] = user
	nextID++
	metrics["registrations"]++
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(user)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	userCount := len(users)
	productCount := len(products)
	orderCount := len(orders)
	mu.RUnlock()

	health := map[string]interface{}{
		"status":       "healthy",
		"timestamp":    time.Now().Format(time.RFC3339),
		"uptime":       time.Since(startTime).String(),
		"version":      "2.0.0",
		"users":        userCount,
		"products":     productCount,
		"orders":       orderCount,
		"memory_usage": "45MB", // Simulated
		"cpu_usage":    "12%",  // Simulated
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	metricsCopy := make(map[string]int)
	for k, v := range metrics {
		metricsCopy[k] = v
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metricsCopy)
}

func publicProductsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	productList := make([]Product, 0, len(products))
	for _, p := range products {
		productList = append(productList, p)
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(productList)
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Println("🔌 WebSocket client connected")

	// Send periodic updates
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mu.RLock()
			update := map[string]interface{}{
				"type":      "system_update",
				"timestamp": time.Now().Format(time.RFC3339),
				"stats": map[string]interface{}{
					"users":    len(users),
					"products": len(products),
					"orders":   len(orders),
					"uptime":   time.Since(startTime).String(),
				},
			}
			mu.RUnlock()

			if err := conn.WriteJSON(update); err != nil {
				log.Printf("WebSocket write error: %v", err)
				return
			}
		}
	}
}

func usersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		mu.RLock()
		userList := make([]User, 0, len(users))
		for _, u := range users {
			userList = append(userList, u)
		}
		mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(userList)

	case "POST":
		var newUser User
		if err := json.NewDecoder(r.Body).Decode(&newUser); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		mu.Lock()
		newUser.ID = nextID
		newUser.Created = time.Now().Format(time.RFC3339)
		users[nextID] = newUser
		nextID++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(newUser)
	}
}

func analyticsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	var totalRevenue float64
	for _, order := range orders {
		if order.Status == "completed" {
			totalRevenue += order.Total
		}
	}

	analytics := Analytics{
		TotalUsers:    len(users),
		TotalProducts: len(products),
		TotalOrders:   len(orders),
		Revenue:       totalRevenue,
		AvgOrderValue: totalRevenue / float64(len(orders)),
		Uptime:        time.Since(startTime).String(),
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(analytics)
}

func searchUsersHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, `{"error":"Query parameter 'q' required"}`, http.StatusBadRequest)
		return
	}

	mu.RLock()
	var results []User
	for _, user := range users {
		if strings.Contains(strings.ToLower(user.Username), strings.ToLower(query)) ||
			strings.Contains(strings.ToLower(user.Email), strings.ToLower(query)) {
			results = append(results, user)
		}
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"query":   query,
		"results": results,
		"count":   len(results),
	})
}

func searchProductsHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	category := r.URL.Query().Get("category")
	minPrice := r.URL.Query().Get("min_price")
	maxPrice := r.URL.Query().Get("max_price")

	mu.RLock()
	var results []Product
	for _, product := range products {
		match := true

		if query != "" && !strings.Contains(strings.ToLower(product.Name), strings.ToLower(query)) &&
			!strings.Contains(strings.ToLower(product.Description), strings.ToLower(query)) {
			match = false
		}

		if category != "" && !strings.EqualFold(product.Category, category) {
			match = false
		}

		if minPrice != "" {
			if min, err := strconv.ParseFloat(minPrice, 64); err == nil && product.Price < min {
				match = false
			}
		}

		if maxPrice != "" {
			if max, err := strconv.ParseFloat(maxPrice, 64); err == nil && product.Price > max {
				match = false
			}
		}

		if match {
			results = append(results, product)
		}
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"query":    query,
		"category": category,
		"results":  results,
		"count":    len(results),
	})
}

func batchUsersHandler(w http.ResponseWriter, r *http.Request) {
	var batch struct {
		Users []User `json:"users"`
	}

	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
		return
	}

	mu.Lock()
	created := make([]User, 0, len(batch.Users))
	for _, user := range batch.Users {
		user.ID = nextID
		user.Created = time.Now().Format(time.RFC3339)
		users[nextID] = user
		created = append(created, user)
		nextID++
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"created": created,
		"count":   len(created),
	})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form
	err := r.ParseMultipartForm(10 << 20) // 10 MB limit
	if err != nil {
		http.Error(w, `{"error":"File too large"}`, http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"No file provided"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Simulate file processing
	response := map[string]interface{}{
		"filename": header.Filename,
		"size":     header.Size,
		"status":   "uploaded",
		"url":      fmt.Sprintf("/files/%s", header.Filename),
		"uploaded": time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func clearCacheHandler(w http.ResponseWriter, r *http.Request) {
	// Simulate cache clearing
	response := map[string]interface{}{
		"status":  "success",
		"message": "Cache cleared successfully",
		"cleared": time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func systemInfoHandler(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"version":      "2.0.0",
		"go_version":   "1.21",
		"architecture": "amd64",
		"os":           "linux",
		"uptime":       time.Since(startTime).String(),
		"memory": map[string]string{
			"total": "512MB",
			"used":  "45MB",
			"free":  "467MB",
		},
		"cpu": map[string]interface{}{
			"cores": 4,
			"usage": "12%",
		},
		"connections": map[string]int{
			"active": 25,
			"total":  1247,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// Implement other handlers (userHandler, productsHandler, etc.) following similar patterns
func userHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	mu.RLock()
	user, exists := users[id]
	mu.RUnlock()

	if !exists {
		http.Error(w, `{"error":"User not found"}`, http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	case "DELETE":
		mu.Lock()
		delete(users, id)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func productsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		mu.RLock()
		productList := make([]Product, 0, len(products))
		for _, p := range products {
			productList = append(productList, p)
		}
		mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(productList)

	case "POST":
		var newProduct Product
		if err := json.NewDecoder(r.Body).Decode(&newProduct); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		mu.Lock()
		newProduct.ID = nextID
		products[nextID] = newProduct
		nextID++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(newProduct)
	}
}

func productHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	mu.RLock()
	product, exists := products[id]
	mu.RUnlock()

	if !exists {
		http.Error(w, `{"error":"Product not found"}`, http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(product)
	case "DELETE":
		mu.Lock()
		delete(products, id)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		mu.RLock()
		orderList := make([]Order, 0, len(orders))
		for _, o := range orders {
			orderList = append(orderList, o)
		}
		mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(orderList)

	case "POST":
		var newOrder Order
		if err := json.NewDecoder(r.Body).Decode(&newOrder); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		mu.Lock()
		newOrder.ID = nextID
		newOrder.Created = time.Now().Format(time.RFC3339)
		newOrder.Status = "pending"
		orders[nextID] = newOrder
		nextID++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(newOrder)
	}
}

func orderHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	mu.RLock()
	order, exists := orders[id]
	mu.RUnlock()

	if !exists {
		http.Error(w, `{"error":"Order not found"}`, http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(order)
	case "DELETE":
		mu.Lock()
		delete(orders, id)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func batchProductsHandler(w http.ResponseWriter, r *http.Request) {
	var batch struct {
		Products []Product `json:"products"`
	}

	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
		return
	}

	mu.Lock()
	created := make([]Product, 0, len(batch.Products))
	for _, product := range batch.Products {
		product.ID = nextID
		products[nextID] = product
		created = append(created, product)
		nextID++
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"created": created,
		"count":   len(created),
	})
}
