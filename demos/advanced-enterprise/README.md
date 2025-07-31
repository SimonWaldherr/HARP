# Advanced Enterprise Demo

This is the most complex HARP backend demo featuring:

## 🌟 Features

### Authentication & Authorization
- User registration and login
- Token-based authentication
- Role-based access control
- Session management

### CRUD Operations
- **Users**: Create, Read, Update, Delete users
- **Products**: Full product management with categories
- **Orders**: Order processing and tracking

### Advanced Functionality
- **WebSocket Support**: Real-time system updates
- **Search**: Advanced search with filters for users and products  
- **Batch Operations**: Bulk create users and products
- **File Upload**: Handle file uploads with size limits
- **Analytics**: Revenue tracking and system metrics
- **Admin Tools**: Cache management and system info

### Monitoring & Health
- Health check endpoint with system stats
- Metrics collection and reporting
- System uptime and resource monitoring

## 🚀 API Endpoints

### Public Endpoints (No Auth)
```
POST /api/auth/login       - User login
POST /api/auth/register    - User registration  
GET  /api/health          - System health check
GET  /api/metrics         - System metrics
GET  /api/products        - Public product listing
WS   /api/ws             - WebSocket connection
```

### Protected Endpoints (Auth Required)
```
Users:
GET    /api/users              - List all users
POST   /api/users              - Create new user
GET    /api/users/{id}          - Get user by ID
PUT    /api/users/{id}          - Update user
DELETE /api/users/{id}          - Delete user

Products:
GET    /api/products            - List all products
POST   /api/products            - Create new product
GET    /api/products/{id}       - Get product by ID
PUT    /api/products/{id}       - Update product
DELETE /api/products/{id}       - Delete product

Orders:
GET    /api/orders              - List all orders
POST   /api/orders              - Create new order
GET    /api/orders/{id}         - Get order by ID
PUT    /api/orders/{id}         - Update order
DELETE /api/orders/{id}         - Delete order

Analytics:
GET /api/analytics              - System analytics

Search:
GET /api/search/users?q=term    - Search users
GET /api/search/products?q=term&category=cat&min_price=10&max_price=100

Batch Operations:
POST /api/batch/users           - Bulk create users
POST /api/batch/products        - Bulk create products

File Operations:
POST /api/upload                - Upload files

Admin:
POST /api/admin/clear-cache     - Clear system cache
GET  /api/admin/system-info     - Detailed system info
```

## 🧪 Testing Examples

### 1. Register a new user:
```bash
curl -X POST http://localhost:8080/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"testuser","email":"test@example.com","password":"password123"}'
```

### 2. Login:
```bash
curl -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"password123"}'
```

### 3. Use the token to access protected endpoints:
```bash
curl -H "Authorization: Bearer YOUR_TOKEN" \
  http://localhost:8080/api/users
```

### 4. Create a product:
```bash
curl -X POST http://localhost:8080/api/products \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"New Product","description":"Amazing product","price":99.99,"stock":10,"category":"Electronics"}'
```

### 5. Search products:
```bash
curl "http://localhost:8080/api/search/products?q=laptop&category=Electronics&min_price=100"
```

### 6. WebSocket Connection:
```javascript
const ws = new WebSocket('ws://localhost:8080/api/ws');
ws.onmessage = (event) => {
  const data = JSON.parse(event.data);
  console.log('Real-time update:', data);
};
```

### 7. File Upload:
```bash
curl -X POST http://localhost:8080/api/upload \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -F "file=@/path/to/your/file.txt"
```

### 8. Batch Operations:
```bash
curl -X POST http://localhost:8080/api/batch/users \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"users":[{"username":"user1","email":"user1@test.com","role":"user"},{"username":"user2","email":"user2@test.com","role":"manager"}]}'
```

## 🔧 Default Credentials
- **Username**: admin
- **Password**: password123

## 📊 Sample Data
The demo starts with:
- 3 users (admin, john, jane)
- 3 products (laptop, mouse, keyboard)  
- 1 completed order

## 🎯 Use Cases
This enterprise demo showcases:
- E-commerce backend functionality
- User management systems
- Inventory management
- Order processing
- Real-time notifications
- API documentation and testing
- Enterprise-grade features like batch operations and file uploads
