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
const serviceName = "auth-service"

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

func validateHandler(w http.ResponseWriter, r *http.Request){
	ctx := otel.GetTextMapPropagator().Extract(r.Context(),propagation.HeaderCarrier(r.Header))
	ctx, span := tracer.Start(ctx,"validate",
		trace.WithAttributes(
			attribute.String("http.method",r.Method),
			attribute.String("http.route","/validate"),
			attribute.String("service.name",serviceName),
		),
	)
	defer span.End()

	userID := r.URL.Query().Get("user_id")
	span.SetAttributes(attribute.String("user_id",userID))

	// Simulate auth check latency
	time.Sleep(50 * time.Millisecond)
 
	// Simulate: deny users with "invalid" in their ID
	valid := userID != "" && userID != "invalid-user"
	if !valid {
		span.SetStatus(codes.Error, "user validation failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":   false,
			"user_id": userID,
			"reason":  "user not found or invalid",
		})
		return
	}
	span.SetAttributes(attribute.Bool("auth.result", true))
	span.SetStatus(codes.Ok, "user validated")
 
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"valid":   true,
		"user_id": userID,
		"role":    "customer",
		"token":   "eyJhbGciOiJIUzI1NiJ9.demo-token",
	})
	log.Printf("[auth-service] user %s validated successfully", userID)
}

func main() {
	tp, err := initTracer()
	if err != nil {
		log.Fatalf("failed to init tracer: %v", err)
	}
	defer tp.Shutdown(context.Background())
 
	tracer = otel.Tracer(serviceName)
 
	http.HandleFunc("/validate", validateHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"auth-service"}`))
	})
 
	log.Println("[auth-service] listening on :8081")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal(err)
	}
}