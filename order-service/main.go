package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const serviceName = "order-service"

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

// injectContext injects trace context into outgoing HTTP request
func injectContext(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// callService makes a downstream HTTP call with trace propagation
func callService(ctx context.Context, url string) (map[string]interface{}, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	injectContext(ctx, req)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, resp.StatusCode, nil
}

func createHandler(w http.ResponseWriter, r *http.Request) {
	// Extract incoming trace context (W3C TraceContext from API Gateway)
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	ctx, span := tracer.Start(ctx, "create",
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", "/create"),
			attribute.String("service.name", serviceName),
		),
	)
	defer span.End()

	orderID := r.URL.Query().Get("order_id")
	userID := r.URL.Query().Get("user_id")
	span.SetAttributes(
		attribute.String("order_id", orderID),
		attribute.String("user_id", userID),
		attribute.Float64("order.amount", 142.50),
		attribute.Int("order.item_count", 3),
	)

	log.Printf("[order-service] /create called — order=%s user=%s", orderID, userID)

	// Step 1: Call LLM Service for payment approval
	llmCtx, llmSpan := tracer.Start(ctx, "call-llm-service")
	llmResult, llmStatus, llmErr := callService(llmCtx,
		fmt.Sprintf("http://localhost:8083/process?order_id=%s&user_id=%s", orderID, userID))

	if llmErr != nil {
		llmSpan.RecordError(llmErr)
		llmSpan.SetStatus(codes.Error, "LLM service unreachable")
		llmSpan.End()
		span.RecordError(llmErr)
		span.SetStatus(codes.Error, "order creation failed: LLM service error")
		http.Error(w, `{"error":"LLM service failed"}`, http.StatusInternalServerError)
		return
	}

	llmSpan.SetAttributes(
		attribute.Int("http.status_code", llmStatus),
		attribute.String("llm.decision", fmt.Sprintf("%v", llmResult["decision"])),
	)

	if llmStatus != http.StatusOK {
		llmSpan.SetStatus(codes.Error, "LLM rejected order")
		llmSpan.End()
		span.SetStatus(codes.Error, "payment rejected")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "rejected",
			"order_id": orderID,
			"reason":   llmResult["reasoning"],
		})
		return
	}
	llmSpan.SetStatus(codes.Ok, "LLM approved")
	llmSpan.End()

	// Step 2: Call Inventory Service to reserve stock
	invCtx, invSpan := tracer.Start(ctx, "call-inventory-service")
	invResult, invStatus, invErr := callService(invCtx,
		fmt.Sprintf("http://localhost:8084/reserve?order_id=%s&item_id=item-001", orderID))

	if invErr != nil {
		invSpan.RecordError(invErr)
		invSpan.SetStatus(codes.Error, "inventory service unreachable")
		invSpan.End()
		span.RecordError(invErr)
		span.SetStatus(codes.Error, "order creation failed: inventory error")
		http.Error(w, `{"error":"inventory service failed"}`, http.StatusInternalServerError)
		return
	}

	invSpan.SetAttributes(
		attribute.Int("http.status_code", invStatus),
		attribute.Bool("inventory.reserved", invResult["reserved"] == true),
	)

	if invStatus != http.StatusOK {
		invSpan.SetStatus(codes.Error, "inventory reservation failed")
		invSpan.End()
		span.SetStatus(codes.Error, "out of stock")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "failed",
			"order_id": orderID,
			"reason":   invResult["reason"],
		})
		return
	}
	invSpan.SetStatus(codes.Ok, "inventory reserved")
	invSpan.End()

	span.SetStatus(codes.Ok, "order created successfully")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "created",
		"order_id":  orderID,
		"user_id":   userID,
		"amount":    142.50,
		"payment":   llmResult,
		"inventory": invResult,
	})
	log.Printf("[order-service] order %s created successfully", orderID)
}

func main() {
	tp, err := initTracer()
	if err != nil {
		log.Fatalf("failed to init tracer: %v", err)
	}
	defer tp.Shutdown(context.Background())

	tracer = otel.Tracer(serviceName)

	http.HandleFunc("/create", createHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"order-service"}`))
	})

	log.Println("[order-service] listening on :8082")
	if err := http.ListenAndServe(":8082", nil); err != nil {
		log.Fatal(err)
	}
}