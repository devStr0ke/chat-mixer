package main

import (
	"log"
	"os"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/handlers"
	"github.com/devstr0ke/chat-mixer/middleware"
	"github.com/devstr0ke/chat-mixer/workers"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal("DB_URL is not set")
	}
	db.Connect(dbURL)
	defer db.DB.Close()
	db.Migrate()

	handlers.WSHub = handlers.NewHub()
	workers.StartExpirationWorker(db.DB, handlers.WSHub.CloseRoom)

	r := gin.Default()

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	auth := r.Group("/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
	}

	protected := middleware.AuthRequired()

	pool := r.Group("/pool", protected)
	{
		pool.POST("/join", handlers.JoinPool)
		pool.POST("/leave", handlers.LeavePool)
	}

	rooms := r.Group("/rooms", protected)
	{
		rooms.GET("/:room_id", handlers.GetRoom)
		rooms.GET("/:room_id/messages", handlers.GetMessages)
	}

	r.GET("/ws/:room_id", protected, handlers.HandleWebSocket)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("server: starting on :%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("server: %v", err)
	}
}
