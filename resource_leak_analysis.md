# Análisis de Posibles Resource Leaks

## Resumen Ejecutivo 

He analizado el código de `flowtest_workers` y encontré **varios resource leaks confirmados y potenciales** que deben ser corregidos.

---

## 🔴 Problemas Críticos Encontrados

### 1. **Goroutine Leak en [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go) - [dispatch()](file:///home/user/flowtest_workers/worker/manager.go#484-494)**
**Archivo:** [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L269-L295)  
**Severidad:** 🔴 CRÍTICA

#### Problema:
```go
func (m *Manager) dispatch(ctx context.Context, workerID string, request *ProxyRequest) (*ProxyResponse, error) {
    // ...
    job := &job{
        request:   request,
        responseC: make(chan *ProxyResponse, 1),  // Canal creado
    }

    select {
    case worker.jobs <- job:
    case <-ctx.Done():
        return nil, ErrRequestCancelled  // ⚠️ Canal nunca cerrado
    }

    select {
    case <-ctx.Done():
        worker.unregisterPending(request.ID)
        return nil, ErrRequestCancelled  // ⚠️ Canal nunca cerrado
    case resp := <-job.responseC:
        return resp, nil
    }
}
```

#### ¿Por qué es un leak?
El canal `job.responseC` se crea pero **nunca se cierra**. Si el contexto se cancela antes de que el worker responda, el canal queda huérfano en memoria y no será recolectado por el GC.

#### Impacto:
- **Memory leak:** Cada request cancelado deja un canal de 1 buffer en memoria
- **Acumulación progresiva:** En sistemas con alta carga y timeouts frecuentes, puede consumir memoria significativa

#### Solución Recomendada:
```go
func (m *Manager) dispatch(ctx context.Context, workerID string, request *ProxyRequest) (*ProxyResponse, error) {
    // ...
    job := &job{
        request:   request,
        responseC: make(chan *ProxyResponse, 1),
    }
    defer close(job.responseC)  // ✅ Cerrar canal al salir

    // resto del código...
}
```

---

### 2. **Channel Leak en [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go) - Worker.jobs**
**Archivo:** [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L110)  
**Severidad:** 🔴 CRÍTICA

#### Problema:
```go
func (m *Manager) Register(suiteID, name string) *Worker {
    worker := &Worker{
        // ...
        jobs: make(chan *job, 32),  // Canal creado
        // ...
    }
    // ...
    return worker
}
```

El canal `worker.jobs` se crea cuando se registra un worker pero **nunca se cierra** cuando el worker se elimina.

#### Código de eliminación:
```go
func (m *Manager) removeWorkerLocked(workerID string) {
    worker, ok := m.workers[workerID]
    if !ok {
        return
    }

    delete(m.workers, workerID)  // ⚠️ Worker eliminado pero canal no cerrado
    // ... resto del código de limpieza ...
}
```

#### Impacto:
- **Channel leak:** Cada worker eliminado deja un canal abierto con buffer de 32
- **Goroutine leak potencial:** Si hay goroutines leyendo/escribiendo en ese canal
- **Memory leak:** Los jobs pendientes en el canal no serán liberados

#### Solución Recomendada:
```go
func (m *Manager) removeWorkerLocked(workerID string) {
    worker, ok := m.workers[workerID]
    if !ok {
        return
    }

    // ✅ Cerrar canal antes de eliminar
    close(worker.jobs)

    delete(m.workers, workerID)
    
    // resto del código...
}
```

**Advertencia:** Cerrar el canal puede causar panic si hay goroutines intentando escribir. Necesitas agregar recover o verificar que no haya escritores activos.

---

### 3. **HTTP Response Body No Cerrado en [main.go](file:///home/user/flowtest_workers/main.go) - [registerWorker()](file:///home/user/flowtest_workers/main.go#98-161)**
**Archivo:** [main.go](file:///home/user/flowtest_workers/main.go#L115-L128)  
**Severidad:** 🟡 MEDIA-ALTA

#### Problema:
```go
func registerWorker(ctx context.Context, client *http.Client, ...) (string, error) {
    // ...
    for {
        // ...
        resp, err := client.Do(req)
        if err != nil {
            if ctx.Err() != nil {
                return "", ctx.Err()  // ⚠️ resp puede estar nil, pero si no lo está...
            }
            log.Printf("register worker: %v; retrying in 5s", err)
            if !sleepWithContext(ctx, 5*time.Second) {
                return "", ctx.Err()  // ⚠️ Salida sin verificar resp
            }
            continue
        }

        data, _ := io.ReadAll(resp.Body)
        resp.Body.Close()  // ✅ Cerrado aquí, pero solo si no hay error
        // ...
    }
}
```

#### ¿Cuál es el problema?
Si `client.Do(req)` retorna un error **y** un `resp` no-nil (válido en Go cuando hay errores de redirect u otros escenarios), el `resp.Body` **no se cierra** en los casos de continue/return dentro del `if err != nil`.

#### Documentación de Go:
> On error, any Response can be ignored. A non-nil Response with a non-nil error only occurs when CheckRedirect fails, and even then the returned Response.Body is already closed.

En este caso específico, el `CheckRedirect` está configurado en línea 51 de [main.go](file:///home/user/flowtest_workers/main.go#L51), pero solo para `targetClient`, no para `serverClient` (línea 48).

#### Impacto:
- **Connection leak:** Conexiones TCP/TLS no liberadas
- **File descriptor leak:** En sistemas con muchas conexiones, puede agotar descriptores
- **Nivel de riesgo:** BAJO en la práctica porque el error típicamente viene con Body cerrado, pero es una mala práctica

#### Solución Recomendada:
```go
resp, err := client.Do(req)
if resp != nil {
    defer resp.Body.Close()  // ✅ Siempre cerrar si resp != nil
}
if err != nil {
    // manejar error...
}
```

---

### 4. **Mismo Problema en [pullRequest()](file:///home/user/flowtest_workers/main.go#173-200), [submitResponse()](file:///home/user/flowtest_workers/main.go#242-263)**
**Archivos:** 
- [main.go](file:///home/user/flowtest_workers/main.go#L173-L199) - [pullRequest()](file:///home/user/flowtest_workers/main.go#173-200)
- [main.go](file:///home/user/flowtest_workers/main.go#L242-L262) - [submitResponse()](file:///home/user/flowtest_workers/main.go#242-263)

**Severidad:** 🟡 MEDIA

#### Problema:
Aunque estas funciones tienen `defer resp.Body.Close()`, lo hacen **después** de verificar el error:

```go
func pullRequest(ctx context.Context, client *http.Client, server, workerID string) (*ProxyRequest, error) {
    // ...
    resp, err := client.Do(req)
    if err != nil {
        return nil, err  // ⚠️ Si resp != nil, no se cierra
    }
    defer resp.Body.Close()  // Solo se ejecuta si err == nil
    // ...
}
```

#### Solución:
Mismo patrón que arriba - mover el `defer` antes del check de error.

---

### 5. **HTTP Request Body No Cerrado en [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go) - `proxyTransport.RoundTrip()`**
**Archivo:** [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L438-L481)  
**Severidad:** 🟢 BAJA (pero es mala práctica)

#### Problema:
```go
func (t *proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    body := []byte{}
    if req.Body != nil {
        data, err := io.ReadAll(req.Body)  // ⚠️ Lee pero no cierra
        if err != nil {
            return nil, err
        }
        body = data
    }
    // ...
}
```

#### ¿Por qué es un problema?
Según la documentación de `http.RoundTripper`:
> RoundTrip should not modify the request, except for consuming and closing the Request's Body.

El código **consume** el body pero **no lo cierra**.

#### Impacto:
- **Nivel bajo:** El caller típicamente cierra el body, pero es responsabilidad del RoundTripper
- **Puede causar warnings/errores** en pruebas o con ciertos HTTP clients

#### Solución:
```go
if req.Body != nil {
    defer req.Body.Close()  // ✅ Cerrar según especificación
    data, err := io.ReadAll(req.Body)
    if err != nil {
        return nil, err
    }
    body = data
}
```

---

## 🟡 Problemas Potenciales de Memoria

### 6. **Map Growth Sin Límites en [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go)**
**Archivo:** [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L26-L29)  
**Severidad:** 🟡 MEDIA

#### Problema:
```go
type Manager struct {
    mu      sync.RWMutex
    workers map[string]*Worker      // ⚠️ Puede crecer indefinidamente
    bySuite map[string]map[string]*workerPool  // ⚠️ Mapa anidado sin límite
}
```

#### ¿Por qué es un problema?
- Los workers "stale" se eliminan en [pruneLocked()](file:///home/user/flowtest_workers/worker/manager.go#382-389) pero **solo cuando se llama explícitamente**
- El pruning se hace en [List()](file:///home/user/flowtest_workers/worker/manager.go#133-157), [hasWorker()](file:///home/user/flowtest_workers/worker/manager.go#324-339), y [nextWorker()](file:///home/user/flowtest_workers/worker/manager.go#340-381), pero **no en un background goroutine**
- Si nadie llama estas funciones, workers viejos **nunca se eliminan**

#### Escenario de leak:
1. Worker se registra
2. Worker se desconecta/muere
3. Nadie llama [List()](file:///home/user/flowtest_workers/worker/manager.go#133-157) o [nextWorker()](file:///home/user/flowtest_workers/worker/manager.go#340-381)
4. Worker permanece en memoria indefinidamente

#### Solución Recomendada:
Agregar goroutine de limpieza periódica:
```go
func NewManager() *Manager {
    m := &Manager{
        workers: map[string]*Worker{},
        bySuite: map[string]map[string]*workerPool{},
    }
    
    // ✅ Goroutine de limpieza periódica
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            m.mu.Lock()
            m.pruneLocked(time.Now())
            m.mu.Unlock()
        }
    }()
    
    return m
}
```

**Importante:** Esta goroutine también es un leak si el Manager nunca se destruye, necesitas mecanismo de shutdown.

---

### 7. **Pending Jobs Sin Cleanup en Worker**
**Archivo:** [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L82)  
**Severidad:** 🟡 MEDIA

#### Problema:
```go
type Worker struct {
    // ...
    pending map[string]*job  // ⚠️ Jobs que nunca completan
}
```

Cuando un worker se elimina en [removeWorkerLocked()](file:///home/user/flowtest_workers/worker/manager.go#390-429), **no se limpian los pending jobs**:

```go
func (m *Manager) removeWorkerLocked(workerID string) {
    worker, ok := m.workers[workerID]
    if !ok {
        return
    }

    delete(m.workers, workerID)
    // ⚠️ worker.pending NO se limpia
    // ⚠️ Los goroutines esperando respuestas quedarán bloqueados
}
```

#### Impacto:
- **Memory leak:** Cada job tiene un [ProxyRequest](file:///home/user/flowtest_workers/worker/manager.go#52-60) y `responseC` que no se liberan
- **Goroutine leak:** Goroutines esperando en `job.responseC` nunca reciben respuesta

#### Solución:
```go
func (m *Manager) removeWorkerLocked(workerID string) {
    worker, ok := m.workers[workerID]
    if !ok {
        return
    }

    // ✅ Cancelar todos los pending jobs
    worker.mu.Lock()
    for _, job := range worker.pending {
        select {
        case job.responseC <- &ProxyResponse{
            RequestID: job.request.ID,
            Error:     "worker disconnected",
        }:
        default:
        }
    }
    worker.pending = nil
    worker.mu.Unlock()

    delete(m.workers, workerID)
    // ...
}
```

---

## 🟢 Buenas Prácticas Observadas

### ✅ Aspectos Positivos:

1. **Contextos manejados correctamente** en [main.go](file:///home/user/flowtest_workers/main.go#L45-L46)
   ```go
   ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
   defer cancel()
   ```

2. **Timer correctamente limpiado** en [main.go](file:///home/user/flowtest_workers/main.go#L163-L164)
   ```go
   timer := time.NewTimer(delay)
   defer timer.Stop()
   ```

3. **Ticker limpiado** en [worker/manager.go](file:///home/user/flowtest_workers/worker/manager.go#L222-L223)
   ```go
   ticker := time.NewTicker(workerHeartbeatInterval)
   defer ticker.Stop()
   ```

4. **HTTP clients reutilizados** - No se crean nuevos clientes en cada request

---

## 📋 Resumen de Prioridades

| # | Problema | Severidad | Impacto | Esfuerzo |
|---|----------|-----------|---------|----------|
| 1 | Channel leak en [dispatch()](file:///home/user/flowtest_workers/worker/manager.go#484-494) | 🔴 Alta | Alto - memory leak acumulativo | Bajo |
| 2 | Channel leak en [removeWorkerLocked()](file:///home/user/flowtest_workers/worker/manager.go#390-429) | 🔴 Alta | Alto - múltiples leaks | Medio |
| 7 | Pending jobs sin cleanup | 🔴 Alta | Goroutine + memory leak | Medio |
| 3-4 | HTTP Response Body no cerrado | 🟡 Media | Connection leaks | Bajo |
| 5 | HTTP Request Body no cerrado | 🟡 Media | Mala práctica | Bajo |
| 6 | Map growth sin límites | 🟡 Media | Memory leak gradual | Medio |

---

## 🔧 Recomendaciones de Implementación

### Orden Sugerido:
1. **Primero:** Arreglar channel leaks (#1, #2) - Son los más peligrosos
2. **Segundo:** Limpiar pending jobs (#7) - Evita goroutine leaks
3. **Tercero:** Agregar background pruning (#6) - Previene growth sin control
4. **Cuarto:** Arreglar HTTP body leaks (#3-5) - Completitud

### Testing Recomendado:
- **Pruebas de stress** con muchos workers registrándose/desconectándose
- **Pruebas de timeout** para verificar cleanup de requests cancelados
- **Memory profiling** con `pprof` para confirmar que no hay leaks
- **Goroutine profiling** para detectar goroutines huérfanas

---

## 📚 Referencias Útiles

- [Effective Go - Defer, Panic, and Recover](https://golang.org/doc/effective_go#defer)
- [Go Blog - Concurrency Patterns](https://blog.golang.org/pipelines)
- [HTTP Client Best Practices](https://golang.org/pkg/net/http/#Client)
