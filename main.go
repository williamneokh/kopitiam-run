package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	_ "strings"
	"time"

	"github.com/glebarez/sqlite" // Pure go driver
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
	"gorm.io/gorm"
)

//go:embed templates/* static/*
var embeddedFS embed.FS

var (
	db           *gorm.DB
	templates    *template.Template
	redirectMode bool
	redirectURL  string
)

// Models
type Room struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

type Order struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RoomID    string    `gorm:"index" json:"room_id"`
	Nickname  string    `json:"nickname"`
	DrinkType string    `json:"drink_type"` // brewed, others, canned, writein
	DrinkName string    `json:"drink_name"`
	Delivered bool      `gorm:"default:false" json:"delivered"`
	CreatedAt time.Time `json:"created_at"`
}

// DTOs
type OrderRequest struct {
	Nickname  string `json:"nickname"`
	DrinkType string `json:"drink_type"`
	DrinkName string `json:"drink_name"`
}

type ConsolidatedDrink struct {
	Name  string
	Count int
	IDs   []uint
}

type UserDrink struct {
	OrderID   uint
	Nickname  string
	DrinkName string
	Delivered bool
}

func main() {
	// Initialize database
	var err error

	// Determine database path. Use environment variable for production,
	// default to a local file for development.
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "kopitiam.db" // Default for local development
	}
	log.Printf("✓ Using database at: %s", dbPath)

	// On a deployment server, we store the database in a persistent volume,
	// which we will mount at /data.
	db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	log.Println("✓ Database connected")

	// Auto migrate
	if err := db.AutoMigrate(&Room{}, &Order{}); err != nil {
		log.Fatal("Failed to migrate database:", err)
	}
	log.Println("✓ Database migrated")

	// Parse templates
	templatesFS, _ := fs.Sub(embeddedFS, "templates")
	templates = template.Must(template.ParseFS(templatesFS, "*.html", "*.template"))
	log.Println("✓ Templates loaded")

	// Check for redirect mode
	redirectMode = os.Getenv("REDIRECT_MODE") == "true"
	redirectURL = os.Getenv("REDIRECT_URL")
	if redirectMode {
		log.Println("🚨 REDIRECT MODE IS ENABLED")
		if redirectURL != "" {
			log.Printf("   Redirect URL: %s", redirectURL)
		}
	}

	// Start the background cleanup job
	go startCleanupJob()

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Serve static files from the embedded filesystem
	staticFS, _ := fs.Sub(embeddedFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Public routes not affected by redirect mode
	r.Get("/health", handleHealthCheck)
	r.Get("/manifest.json", handleManifest)

	// All application routes are subject to redirect mode
	r.Group(func(r chi.Router) {
		r.Use(redirectMiddleware)

		r.Get("/", handleLanding)
		r.Get("/debug", handleDebug)
		r.Post("/room/create", handleCreateRoom)
		r.Get("/order/{roomID}", handleOrderPage)
		r.Post("/order/{roomID}", handleSubmitOrder)
		r.Get("/room/{roomID}/admin", handleAdminView)
		r.Get("/room/{roomID}/orders", handleGetOrders)
		r.Post("/room/{roomID}/orders/{orderID}/toggle", handleToggleDelivered)
		r.Get("/qr/{roomID}", handleQRCode)
	})

	log.Println("🚀 Server starting on http://localhost:8080")
	log.Println("📱 Open http://localhost:8080 in your browser")

	if err := http.ListenAndServe(":8080", r); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}

func redirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirectMode {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			templates.ExecuteTemplate(w, "moved.html", map[string]string{"RedirectURL": redirectURL})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	// A simple health check that just returns 200 OK.
	// This gives Fly.io a lightweight endpoint to confirm the app is running.
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func startCleanupJob() {
	log.Println("🧹 Starting background cleanup job...")
	// Run the cleanup job immediately on start, and then every 7 days.
	ticker := time.NewTicker(7 * 24 * time.Hour)
	defer ticker.Stop()

	// Perform an initial cleanup on startup
	cleanupOldRooms()

	for range ticker.C {
		cleanupOldRooms()
	}
}

func cleanupOldRooms() {
	log.Println("🧹 Running weekly cleanup of old rooms...")
	const retentionPeriod = 7 * 24 * time.Hour // Keep data for 7 days
	cutoff := time.Now().Add(-retentionPeriod)

	var oldRoomIDs []string
	// Find rooms older than the retention period
	if err := db.Model(&Room{}).Where("created_at < ?", cutoff).Pluck("id", &oldRoomIDs).Error; err != nil {
		log.Printf("❌ Error finding old rooms for cleanup: %v", err)
		return
	}

	if len(oldRoomIDs) == 0 {
		log.Println("🧹 No old rooms to clean up.")
		return
	}

	log.Printf("🧹 Found %d old rooms to delete. Deleting associated orders and rooms...", len(oldRoomIDs))
	db.Where("room_id IN ?", oldRoomIDs).Delete(&Order{})
	db.Where("id IN ?", oldRoomIDs).Delete(&Room{})

	log.Println("🧹 Running VACUUM to reclaim disk space...")
	db.Exec("VACUUM")
}

func handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	templates.ExecuteTemplate(w, "manifest.json.template", nil)
}

func handleLanding(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "landing.html", nil)
}

func handleDebug(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "debug.html", nil)
}

func handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	log.Println("📝 Room creation request received")
	log.Printf("   Method: %s", r.Method)
	log.Printf("   HX-Request header: %s", r.Header.Get("HX-Request"))
	log.Printf("   User-Agent: %s", r.Header.Get("User-Agent"))

	roomID := uuid.New().String()[:8]
	log.Printf("   Generated room ID: %s", roomID)

	room := Room{
		ID:        roomID,
		CreatedAt: time.Now(),
	}

	if err := db.Create(&room).Error; err != nil {
		log.Printf("❌ Error creating room: %v", err)
		http.Error(w, "Failed to create room", http.StatusInternalServerError)
		return
	}

	log.Printf("✅ Room created successfully: %s", roomID)

	// For HTMX request
	if r.Header.Get("HX-Request") == "true" {
		log.Printf("   Using HTMX redirect to: /room/%s/admin", roomID)
		w.Header().Set("HX-Redirect", "/room/"+roomID+"/admin")
		w.WriteHeader(http.StatusOK)
		return
	}

	// For regular request (fallback)
	log.Printf("   Using HTTP redirect to: /room/%s/admin", roomID)
	http.Redirect(w, r, "/room/"+roomID+"/admin", http.StatusSeeOther)
}

func handleOrderPage(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")

	// Verify room exists
	var room Room
	if err := db.First(&room, "id = ?", roomID).Error; err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"RoomID": roomID,
	}
	templates.ExecuteTemplate(w, "order.html", data)
}

func handleSubmitOrder(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")

	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	order := Order{
		RoomID:    roomID,
		Nickname:  req.Nickname,
		DrinkType: req.DrinkType,
		DrinkName: req.DrinkName,
	}
	db.Create(&order)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func handleAdminView(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")

	// Verify room exists
	var room Room
	if err := db.First(&room, "id = ?", roomID).Error; err != nil {
		log.Printf("Room not found: %s, error: %v", roomID, err)
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	// Generate order link
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	orderLink := fmt.Sprintf("%s://%s/order/%s", scheme, r.Host, roomID)

	data := map[string]interface{}{
		"RoomID":    roomID,
		"OrderLink": orderLink,
	}

	if err := templates.ExecuteTemplate(w, "admin.html", data); err != nil {
		log.Printf("Error rendering admin template: %v", err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
		return
	}
}

func handleGetOrders(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	view := r.URL.Query().Get("view") // "consolidated" or "distribution"

	var orders []Order
	db.Where("room_id = ?", roomID).Order("created_at asc").Find(&orders)

	if view == "consolidated" {
		consolidated := consolidateOrders(orders)
		templates.ExecuteTemplate(w, "consolidated_view.html", consolidated)
	} else {
		distribution := distributeOrders(orders)
		data := map[string]interface{}{
			"Orders": distribution,
			"RoomID": roomID,
		}
		templates.ExecuteTemplate(w, "distribution_view.html", data)
	}
}

func consolidateOrders(orders []Order) []ConsolidatedDrink {
	drinkMap := make(map[string]*ConsolidatedDrink)

	for _, order := range orders {
		if existing, exists := drinkMap[order.DrinkName]; exists {
			existing.Count++
			existing.IDs = append(existing.IDs, order.ID)
		} else {
			drinkMap[order.DrinkName] = &ConsolidatedDrink{
				Name:  order.DrinkName,
				Count: 1,
				IDs:   []uint{order.ID},
			}
		}
	}

	// Convert map to slice
	result := make([]ConsolidatedDrink, 0, len(drinkMap))
	for _, drink := range drinkMap {
		result = append(result, *drink)
	}

	// Sort the slice by drink name to ensure a stable order on every refresh.
	// This prevents the list from re-ordering on auto-refresh.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

func distributeOrders(orders []Order) []UserDrink {
	result := make([]UserDrink, 0, len(orders))

	for _, order := range orders {
		result = append(result, UserDrink{
			OrderID:   order.ID,
			Nickname:  order.Nickname,
			DrinkName: order.DrinkName,
			Delivered: order.Delivered,
		})
	}

	return result
}

func handleToggleDelivered(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	orderID := chi.URLParam(r, "orderID")

	var order Order
	if err := db.First(&order, "id = ? AND room_id = ?", orderID, roomID).Error; err != nil {
		http.Error(w, "Order not found", http.StatusNotFound)
		return
	}

	order.Delivered = !order.Delivered
	db.Save(&order)

	// After toggling, we need to return the entire updated list for the HTMX swap.
	// This prevents race conditions with the auto-refresh and provides instant feedback.
	var orders []Order
	db.Where("room_id = ?", roomID).Order("created_at asc").Find(&orders)

	distribution := distributeOrders(orders)
	data := map[string]interface{}{
		"Orders": distribution,
		"RoomID": roomID,
	}
	templates.ExecuteTemplate(w, "distribution_view.html", data)
}

func handleQRCode(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	orderLink := fmt.Sprintf("%s://%s/order/%s", scheme, r.Host, roomID)

	qr, err := qrcode.Encode(orderLink, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "Failed to generate QR code", http.StatusInternalServerError)
		return
	}

	// Return base64 encoded image for embedding
	encoded := base64.StdEncoding.EncodeToString(qr)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("data:image/png;base64," + encoded))
}
