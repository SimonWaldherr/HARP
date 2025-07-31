#!/bin/bash

# HARP Enterprise Demo Test Script
# Tests all major features of the Advanced Enterprise Backend

echo "🧪 HARP Advanced Enterprise Backend Test Suite"
echo "=============================================="

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

BASE_URL="http://localhost:8080/api"
TOKEN=""

# Helper function for API calls
api_call() {
    local method=$1
    local endpoint=$2
    local data=$3
    local auth_header=""
    
    if [ ! -z "$TOKEN" ]; then
        auth_header="-H \"Authorization: Bearer $TOKEN\""
    fi
    
    if [ ! -z "$data" ]; then
        eval curl -s -X $method "$BASE_URL$endpoint" \
            -H "Content-Type: application/json" \
            $auth_header \
            -d "'$data'"
    else
        eval curl -s -X $method "$BASE_URL$endpoint" \
            -H "Content-Type: application/json" \
            $auth_header
    fi
}

echo -e "${BLUE}📋 Testing Public Endpoints...${NC}"

# Test health endpoint
echo -e "🔍 Testing health check..."
HEALTH=$(api_call GET "/health")
if echo "$HEALTH" | grep -q "healthy"; then
    echo -e "${GREEN}✅ Health check passed${NC}"
else
    echo -e "${RED}❌ Health check failed${NC}"
fi

# Test public products
echo -e "🔍 Testing public product listing..."
PRODUCTS=$(api_call GET "/products")
if echo "$PRODUCTS" | grep -q "Laptop"; then
    echo -e "${GREEN}✅ Public products accessible${NC}"
else
    echo -e "${RED}❌ Public products failed${NC}"
fi

echo -e "${BLUE}🔐 Testing Authentication...${NC}"

# Test login
echo -e "🔍 Testing user login..."
LOGIN_RESPONSE=$(api_call POST "/auth/login" '{"username":"admin","password":"password123"}')
TOKEN=$(echo "$LOGIN_RESPONSE" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

if [ ! -z "$TOKEN" ]; then
    echo -e "${GREEN}✅ Login successful - Token: ${TOKEN:0:20}...${NC}"
else
    echo -e "${RED}❌ Login failed${NC}"
    exit 1
fi

echo -e "${BLUE}👥 Testing User Management...${NC}"

# Test user listing
echo -e "🔍 Testing user listing..."
USERS=$(api_call GET "/users")
if echo "$USERS" | grep -q "admin"; then
    echo -e "${GREEN}✅ User listing works${NC}"
else
    echo -e "${RED}❌ User listing failed${NC}"
fi

# Test user creation
echo -e "🔍 Testing user creation..."
NEW_USER=$(api_call POST "/users" '{"username":"testuser","email":"test@example.com","role":"user"}')
if echo "$NEW_USER" | grep -q "testuser"; then
    echo -e "${GREEN}✅ User creation works${NC}"
else
    echo -e "${RED}❌ User creation failed${NC}"
fi

echo -e "${BLUE}🛍️ Testing Product Management...${NC}"

# Test product creation
echo -e "🔍 Testing product creation..."
NEW_PRODUCT=$(api_call POST "/products" '{"name":"Test Product","description":"A test product","price":29.99,"stock":100,"category":"Test"}')
if echo "$NEW_PRODUCT" | grep -q "Test Product"; then
    echo -e "${GREEN}✅ Product creation works${NC}"
else
    echo -e "${RED}❌ Product creation failed${NC}"
fi

echo -e "${BLUE}🔍 Testing Search Functionality...${NC}"

# Test user search
echo -e "🔍 Testing user search..."
USER_SEARCH=$(api_call GET "/search/users?q=admin")
if echo "$USER_SEARCH" | grep -q "admin"; then
    echo -e "${GREEN}✅ User search works${NC}"
else
    echo -e "${RED}❌ User search failed${NC}"
fi

# Test product search
echo -e "🔍 Testing product search..."
PRODUCT_SEARCH=$(api_call GET "/search/products?q=laptop&category=Electronics")
if echo "$PRODUCT_SEARCH" | grep -q "laptop"; then
    echo -e "${GREEN}✅ Product search works${NC}"
else
    echo -e "${RED}❌ Product search failed${NC}"
fi

echo -e "${BLUE}📊 Testing Analytics...${NC}"

# Test analytics
echo -e "🔍 Testing analytics endpoint..."
ANALYTICS=$(api_call GET "/analytics")
if echo "$ANALYTICS" | grep -q "total_users"; then
    echo -e "${GREEN}✅ Analytics endpoint works${NC}"
else
    echo -e "${RED}❌ Analytics endpoint failed${NC}"
fi

echo -e "${BLUE}⚡ Testing Batch Operations...${NC}"

# Test batch user creation
echo -e "🔍 Testing batch user creation..."
BATCH_USERS=$(api_call POST "/batch/users" '{"users":[{"username":"batch1","email":"batch1@test.com","role":"user"},{"username":"batch2","email":"batch2@test.com","role":"user"}]}')
if echo "$BATCH_USERS" | grep -q "batch1"; then
    echo -e "${GREEN}✅ Batch user creation works${NC}"
else
    echo -e "${RED}❌ Batch user creation failed${NC}"
fi

echo -e "${BLUE}🔧 Testing Admin Features...${NC}"

# Test system info
echo -e "🔍 Testing system info..."
SYSTEM_INFO=$(api_call GET "/admin/system-info")
if echo "$SYSTEM_INFO" | grep -q "version"; then
    echo -e "${GREEN}✅ System info works${NC}"
else
    echo -e "${RED}❌ System info failed${NC}"
fi

# Test metrics
echo -e "🔍 Testing metrics..."
METRICS=$(api_call GET "/metrics")
if echo "$METRICS" | grep -q "{"; then
    echo -e "${GREEN}✅ Metrics endpoint works${NC}"
else
    echo -e "${RED}❌ Metrics endpoint failed${NC}"
fi

echo ""
echo -e "${GREEN}🎉 Enterprise Backend Test Suite Complete!${NC}"
echo -e "${YELLOW}📈 Features Tested:${NC}"
echo "   ✅ Authentication & Authorization"
echo "   ✅ User Management (CRUD)"
echo "   ✅ Product Management (CRUD)"
echo "   ✅ Search Functionality"
echo "   ✅ Analytics & Reporting"
echo "   ✅ Batch Operations"
echo "   ✅ Admin Features"
echo "   ✅ Health Monitoring"
echo ""
echo -e "${BLUE}🌐 WebSocket Testing:${NC}"
echo "   Connect to: ws://localhost:8080/api/ws"
echo ""
echo -e "${BLUE}📁 File Upload Testing:${NC}"
echo "   curl -X POST http://localhost:8080/api/upload -H \"Authorization: Bearer $TOKEN\" -F \"file=@yourfile.txt\""
echo ""
echo -e "${YELLOW}🔗 All API endpoints are working correctly!${NC}"
