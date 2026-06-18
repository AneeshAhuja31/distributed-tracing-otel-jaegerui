package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
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

const serviceName = "llm-service"

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

func mockLLMDecision(orderID, userID string) (string, string, int64) {
	// Simulate variable LLM latency (200–800ms)
	latency := 200 + rand.Int63n(600)
	time.Sleep(time.Duration(latency) * time.Millisecond)
 
	// Simple deterministic mock: approve unless order ID ends in "9"
	decision := "APPROVED"
	reasoning := fmt.Sprintf("Mock LLM: Order %s for user %s passes risk checks. Amount within threshold.", orderID, userID)
	if len(orderID) > 0 && orderID[len(orderID)-1] == '9' {
		decision = "REJECTED"
		reasoning = fmt.Sprintf("Mock LLM: Order %s flagged for manual review — unusual pattern detected.", orderID)
	}
	return decision, reasoning, latency
}

func callRealLLM(ctx context.Context, orderID, userID, apiKey string)(string,string,int64,error){
	prompt := fmt.Sprintf(
		"You are a payment approval system. Given the following order, respond ONLY with a JSON object with keys 'decision' (APPROVED or REJECTED) and 'reasoning' (one sentence).\n\nOrder ID: %s\nUser ID: %s\nAmount: $142.50\nItems: 3\nRisk score: low",
		orderID, userID,
	)

	reqBody := map[string]interface{}{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 150,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	bodyBytes, _ := json.Marshal(reqBody)
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
 
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", "", latency, err
	}
	defer resp.Body.Close()
 
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", latency, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(body))
	}
 
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", latency, err
	}
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		return "", "", latency, fmt.Errorf("empty LLM response")
	}
	firstBlock, _ := content[0].(map[string]interface{})
	text, _ := firstBlock["text"].(string)
 
	var decision map[string]string
	if err := json.Unmarshal([]byte(text), &decision); err != nil {
		if bytes.Contains([]byte(text), []byte("APPROVED")) {
			return "APPROVED", text, latency, nil
		}
		return "REJECTED", text, latency, nil
	}
 
	return decision["decision"], decision["reasoning"], latency, nil

}

func processHandler(w http.ResponseWriter, r *http.Request) {
	// Extract incoming trace context
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
 
	ctx, span := tracer.Start(ctx, "process",
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", "/process"),
			attribute.String("service.name", serviceName),
		),
	)
	defer span.End()
 
	orderID := r.URL.Query().Get("order_id")
	userID := r.URL.Query().Get("user_id")
	span.SetAttributes(
		attribute.String("order_id", orderID),
		attribute.String("user_id", userID),
	)
 
	log.Printf("[llm-service] /process called — order=%s user=%s", orderID, userID)
 
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
 
	var (
		decision  string
		reasoning string
		latency   int64
		provider  string
		llmErr    error
	)
 
	// Span for the LLM call itself
	_, llmSpan := tracer.Start(ctx, "llm-call")
 
	if apiKey != "" {
		provider = "anthropic"
		llmSpan.SetAttributes(attribute.String("llm.provider", provider))
		log.Printf("[llm-service] calling real LLM (Anthropic)")
		decision, reasoning, latency, llmErr = callRealLLM(ctx, orderID, userID, apiKey)
	} else {
		provider = "mock"
		llmSpan.SetAttributes(attribute.String("llm.provider", provider))
		log.Printf("[llm-service] no API key found — using mock LLM")
		decision, reasoning, latency = mockLLMDecision(orderID, userID)
	}
 
	llmSpan.SetAttributes(
		attribute.Int64("llm.latency_ms", latency),
		attribute.String("llm.decision", decision),
	)
 
	if llmErr != nil {
		llmSpan.RecordError(llmErr)
		llmSpan.SetStatus(codes.Error, "LLM call failed")
		llmSpan.End()
		span.RecordError(llmErr)
		span.SetStatus(codes.Error, "LLM processing failed")
		http.Error(w, `{"error":"LLM processing failed"}`, http.StatusInternalServerError)
		return
	}
	llmSpan.SetStatus(codes.Ok, "LLM call successful")
	llmSpan.End()
 
	span.SetAttributes(
		attribute.String("llm.decision", decision),
		attribute.String("llm.provider", provider),
	)
 
	if decision == "REJECTED" {
		span.SetStatus(codes.Error, "payment rejected by LLM")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":     "rejected",
			"decision":   decision,
			"reasoning":  reasoning,
			"latency_ms": latency,
			"provider":   provider,
		})
		return
	}
 
	span.SetStatus(codes.Ok, "payment approved")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "approved",
		"decision":   decision,
		"reasoning":  reasoning,
		"latency_ms": latency,
		"provider":   provider,
		"order_id":   orderID,
	})
	log.Printf("[llm-service] decision=%s latency=%dms provider=%s", decision, latency, provider)
}
 
func main() {
	tp, err := initTracer()
	if err != nil {
		log.Fatalf("failed to init tracer: %v", err)
	}
	defer tp.Shutdown(context.Background())
 
	tracer = otel.Tracer(serviceName)
 
	http.HandleFunc("/process", processHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"llm-service"}`))
	})
 
	log.Println("[llm-service] listening on :8083")
	if err := http.ListenAndServe(":8083", nil); err != nil {
		log.Fatal(err)
	}
}
 