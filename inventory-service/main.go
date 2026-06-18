package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
 
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)
 
const serviceName = "inventory-service"
 
var tracer trace.Tracer
 
func initTracer() (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpoint("localhost:4318"),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}
 
	res, _ := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
 
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
 
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
 
	return tp, nil
}
var inventory = map[string]int{
	"item-001": 50,
	"item-002": 3,
	"item-003": 0, 
}
 
func reserveHandler(w http.ResponseWriter, r *http.Request) {
	// Extract incoming trace context
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
 
	ctx, span := tracer.Start(ctx, "reserve",
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", "/reserve"),
			attribute.String("service.name", serviceName),
		),
	)
	defer span.End()
 
	orderID := r.URL.Query().Get("order_id")
	itemID := r.URL.Query().Get("item_id")
	if itemID == "" {
		itemID = "item-001" // default
	}
 
	span.SetAttributes(
		attribute.String("order_id", orderID),
		attribute.String("item_id", itemID),
	)
 
	log.Printf("[inventory-service] /reserve called — order=%s item=%s", orderID, itemID)
 
	// Simulate DB lookup latency
	_, dbSpan := tracer.Start(ctx, "db-lookup-inventory")
	time.Sleep(30 * time.Millisecond)
	stock, exists := inventory[itemID]
	dbSpan.SetAttributes(
		attribute.String("db.operation", "SELECT"),
		attribute.String("db.table", "inventory"),
		attribute.String("item_id", itemID),
		attribute.Int("stock.available", stock),
	)
	dbSpan.End()
 
	if !exists {
		span.SetStatus(codes.Error, "item not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"reserved": false,
			"reason":   "item not found",
			"item_id":  itemID,
		})
		return
	}
 
	if stock <= 0 {
		span.SetAttributes(attribute.Bool("inventory.out_of_stock", true))
		span.SetStatus(codes.Error, "out of stock")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"reserved": false,
			"reason":   "out of stock",
			"item_id":  itemID,
		})
		return
	}
 
	// Simulate reservation write
	_, writeSpan := tracer.Start(ctx, "db-reserve-item")
	time.Sleep(20 * time.Millisecond)
	inventory[itemID]-- // decrement stock
	writeSpan.SetAttributes(
		attribute.String("db.operation", "UPDATE"),
		attribute.String("db.table", "inventory"),
		attribute.Int("stock.remaining", inventory[itemID]),
	)
	writeSpan.End()
 
	span.SetAttributes(
		attribute.Bool("inventory.reserved", true),
		attribute.Int("inventory.stock_remaining", inventory[itemID]),
	)
	span.SetStatus(codes.Ok, "item reserved")
 
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"reserved":        true,
		"item_id":         itemID,
		"order_id":        orderID,
		"stock_remaining": inventory[itemID],
	})
	log.Printf("[inventory-service] item %s reserved for order %s", itemID, orderID)
}
 
func main() {
	tp, err := initTracer()
	if err != nil {
		log.Fatalf("failed to init tracer: %v", err)
	}
	defer tp.Shutdown(context.Background())
 
	tracer = otel.Tracer(serviceName)
 
	http.HandleFunc("/reserve", reserveHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"inventory-service"}`))
	})
 
	log.Println("[inventory-service] listening on :8084")
	if err := http.ListenAndServe(":8084", nil); err != nil {
		log.Fatal(err)
	}
}
 