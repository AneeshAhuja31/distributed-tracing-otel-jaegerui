package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	// "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "api-gateway"

var tracer trace.Tracer

func initTracer()(*sdktrace.TracerProvider,error){
	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpoint("localhost:4318"),
		otlptracehttp.WithInsecure(),
	)
	
	if err != nil{
		return nil,fmt.Errorf("failed to create OTPL exporter: %w",err)
	}
	res,_ := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter), //buffers span (sends in batches)
		sdktrace.WithResource(res), // attaches service metadata
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp) //makes provider available gloabally
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)) //enables distributed tracing, injects trace info into HTTP headers and extracts it on incoming requests
	return tp,nil
}

func injectContext(ctx context.Context, req *http.Request){ //injects the current span context into outgoing HTTP request headers
	//same trace continues across downstream services
	otel.GetTextMapPropagator().Inject(ctx,propagation.HeaderCarrier(req.Header))
}

func callService(ctx context.Context,url string)(map[string]interface{},error){
	req,err := http.NewRequestWithContext(ctx, http.MethodGet,url,nil)
	if err != nil{
		return nil,err
	}
	injectContext(ctx,req) //inject trace headers

	client := &http.Client{Timeout: 10*time.Second}
	resp,err := client.Do(req)
	if err != nil{
		return nil,err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil{
		return nil,err
	}
	
	if resp.StatusCode != http.StatusOK{
		return nil, fmt.Errorf("service returned status: %d: %s",resp.StatusCode,string(body))
	}

	var result map[string]interface{}

	if err := json.Unmarshal(body,&result); err!=nil{
		return nil,err
	}
	return result,err
}

func checkoutHandler(w http.ResponseWriter, r *http.Request){
	ctx, span := tracer.Start(r.Context(),"checkout",
    	trace.WithAttributes(
			attribute.String("http.method",r.Method),
			attribute.String("http.route","/checkout"),
			attribute.String("service.name",serviceName),
		),
	)
	defer span.End()

	userID := r.URL.Query().Get("user_id")
	orderID := r.URL.Query().Get("order_id")
	if userID == ""{
		userID = "user-123"
	}
	if orderID == ""{
		orderID = "order-456"
	}
	span.SetAttributes(
		attribute.String("user_id",userID),
		attribute.String("order_id",orderID),
	)

	log.Printf("[api-gateway] /checkout called - user=%s order=%s",userID,orderID)

	// Step 1: Call Auth Service 
	//this is child span of checkout
	authCtx, authSpan := tracer.Start(ctx, "call-auth-service")
	authResult, err := callService(authCtx, fmt.Sprintf("http://localhost:8081/validate?user_id=%s", userID)) //passing authCtx keeps trace chain
	if err != nil{
		authSpan.RecordError(err)
		authSpan.SetStatus(codes.Error,"auth service failed")
		authSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error,"checkout faield at auth")
		http.Error(w,`{"error":"auth service failed"}`,http.StatusInternalServerError)
		return 
	}
	authSpan.SetAttributes(
		attribute.Bool("auth.valid",authResult["valid"]==true),
	)
	authSpan.End()

	if valid,ok := authResult["valid"].(bool); !ok || !valid {
		span.SetStatus(codes.Error,"unauthorized")
		http.Error(w,`{"error":"unauthorized"}`,http.StatusUnauthorized)
		return
	}

	// Step 2: Call Order Service
	//child span of checkout
	orderCtx, orderSpan := tracer.Start(ctx, "call-order-service")
	orderResult, err := callService(orderCtx, fmt.Sprintf("http://localhost:8082/create?order_id=%s&user_id=%s", orderID, userID))
	if err != nil {
		orderSpan.RecordError(err)
		orderSpan.SetStatus(codes.Error, "order service failed")
		orderSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error, "checkout failed at order creation")
		http.Error(w, `{"error":"order service failed"}`, http.StatusInternalServerError)
		return
	}
	orderSpan.SetAttributes(attribute.String("order.status", fmt.Sprintf("%v", orderResult["status"])))
	orderSpan.End()
 
	span.SetStatus(codes.Ok, "checkout complete")

	response := map[string]interface{}{
		"status":"success",
		"order_id":orderID,
		"user_id":userID,
		"auth":authResult,
		"order":orderResult,
	}

	w.Header().Set("Content-Type","application/json")
	json.NewEncoder(w).Encode(response)
	log.Printf("[api-gateway] /checkout completed successfully")
}

func main(){
	tp,err := initTracer()
	if err != nil{
		log.Fatalf("failed to init tracer: %v",err)
	}

	defer tp.Shutdown(context.Background())
	tracer = otel.Tracer(serviceName)

	http.HandleFunc("/checkout",checkoutHandler)
	http.HandleFunc("/health",func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"api-gateway"}`))
	})

	log.Println("[api-gateway] listening on :8080")
	if err := http.ListenAndServe(":8080",nil); err != nil{
		log.Fatalf("Server failed to start: %v", err)
	}
}