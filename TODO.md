# TODO - Telegram Music Searcher Bot

> Comprehensive improvement roadmap organized by priority level

## =� HIGH PRIORITY (Fix Soon)

### Security
- [ ] **Add rate limiting** (main.go)
  - Implement per-user token bucket algorithm
  - Prevent abuse and DoS attacks
  - Add configurable limits via Config struct
  - Location: Bot message handler

- [ ] **Add input sanitization** (main.go)
  - Validate URL schemes (only http/https)
  - Validate domains against allowlist of music platforms
  - Implement request size limits
  - **Risk**: SSRF vulnerability
  - Location: `handleMessage` function

- [ ] **Fix deployment security** (service.tpl, Makefile)
  - Create dedicated service user (not root)
  - Update SystemD service to run as non-root
  - Add service hardening options (NoNewPrivileges, ProtectSystem, etc.)
  - Location: `service.tpl` lines 9-10

### Code Quality
- [ ] **Refactor tryOEmbed function** (main.go:854-980)
  - Extract smaller functions: `parseOEmbedDescription()`, `extractArtistFromTitle()`, `validateAndFallback()`
  - 127 lines is too complex for single function
  - Violates Single Responsibility Principle
  - Makes testing and debugging difficult

### Testing
- [ ] **Add unit tests for complex functions**
  - `TestTryOEmbed` - 6-layer fallback logic
  - `TestExtractWithFallbacks` - Spotify extraction strategy
  - `TestSearchViaDDG` - DuckDuckGo search logic
  - `TestCollectYouTubeCandidatesRecursive` - JSON traversal
  - `TestExtractAppleLD` - JSON-LD parsing
  - `TestHandleMessage` - Main message handler
  - Target: 80%+ code coverage

- [ ] **Fix integration test dependencies** (main_test.go)
  - Add mock HTTP responses for unit tests
  - Keep real integration tests with build tags
  - Add contract tests for API assumptions
  - **Issue**: Tests are slow, flaky, depend on external services

### Documentation
- [ ] **Expand README.md**
  - Add architecture overview
  - Setup instructions
  - Environment variables documentation
  - Deployment guide
  - API rate limits information
  - Development guide
  - Contributing guidelines

## =� MEDIUM PRIORITY (Plan to Fix)

### Code Quality
- [ ] **Create closeResponse helper** (main.go)
  - Consolidate 15 duplicate defer patterns
  - `func closeResponse(resp *http.Response)`
  - Replace: `defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()`

- [ ] **Standardize logging** (throughout main.go)
  - Remove plain `log` package usage (10 occurrences)
  - Use zap.Logger consistently everywhere
  - Pass logger to all components

- [ ] **Convert magic numbers to constants**
  - Line 2369: `3500` � `maxTelegramMessageSize`
  - Line 2446: `max/2` � `messageChunkCutoff`
  - Line 609: `0.6` � `circuitBreakerFailureThreshold`
  - Lines 1821-1843: Scoring weights � Named constants with docs

- [ ] **Improve error wrapping** (throughout main.go)
  - Use `fmt.Errorf` with `%w` consistently
  - Currently only 12 uses out of 62 error checks
  - Preserves error context for debugging

### Performance
- [ ] **Fix thread-safety issue** (main.go:523)
  - `rand.Rand` is not safe for concurrent use
  - **Risk**: Race condition in HTTP retry logic
  - **Solution**: Use `math/rand` global functions (thread-safe)

- [ ] **Add request deduplication**
  - Implement using `golang.org/x/sync/singleflight`
  - Prevents duplicate HTTP calls for same URL
  - **Benefit**: Reduces load when multiple users request same song

- [ ] **Add cache size limits** (main.go:413-481)
  - Current `SimpleCache` has no max size
  - **Risk**: Memory exhaustion under heavy usage
  - **Solution**: Add LRU eviction or max size limit

- [ ] **Optimize YouTube candidate collection** (main.go:1761-1811)
  - Add candidate limit and early exit
  - Limit recursion depth
  - **Issue**: Traverses entire JSON tree without early exit

### Configuration
- [ ] **Enhance config validation** (main.go:128-162)
  - Validate URL format for TextMirrorURL
  - Validate port ranges for HealthPort
  - Add reasonable bounds for timeouts and limits

- [ ] **Fix health check server** (main.go:2767-2787)
  - Add error channel for startup failures
  - Log startup success/failure
  - **Issue**: Errors silently ignored in goroutine

- [ ] **Fix request timeout bug** (main.go:2299)
  - `resolveSpotifyShortLink` uses `10 * http.DefaultClient.Timeout`
  - `http.DefaultClient.Timeout` is 0 (no timeout)
  - **Risk**: Potential goroutine leak
  - **Solution**: Set explicit timeout: `Timeout: 10 * time.Second`

### Security
- [ ] **Rotate User-Agent** (main.go:34, 544, 2307)
  - Same User-Agent used for all requests
  - Easy to identify and block bot traffic
  - Add User-Agent pool with random selection

- [ ] **Sanitize sensitive data in logs**
  - URLs, tokens could be logged in debug mode
  - Redact sensitive fields before logging

### Testing
- [ ] **Add error path testing** (main_test.go)
  - Invalid URL handling
  - Network timeout scenarios
  - Malformed API responses
  - Circuit breaker open states
  - Cache failures
  - Concurrent access scenarios

### Features & UX
- [ ] **Improve error messages** (main.go:2690-2696)
  - Detect playlist/album links � helpful error
  - Region restrictions � specific message
  - Service unavailable � retry suggestion
  - **Issue**: Generic "couldn't process" for all failures

- [ ] **Add playlist/album support**
  - Currently only track URLs supported
  - Detect and handle gracefully or support extraction

### DevOps
- [ ] **Add CI/CD pipeline**
  - Create GitHub Actions workflow
  - Run tests on PR
  - Lint checks (golangci-lint)
  - Security scanning (gosec)
  - Build verification
  - Automated deployment on merge to main

- [ ] **Improve linter configuration** (.golangci.yml)
  - Replace `enable-all: true` with specific linters
  - Disable auto-fix for CI environment
  - Document linter choices

- [ ] **Update Go version** (go.mod:3)
  - Currently requires 1.23.0, toolchain is 1.24.5
  - Update to require Go 1.24 or document version strategy

## =� LOW PRIORITY (Nice to Have)

### Code Quality
- [ ] **Document regex patterns** (main.go:38-67)
  - Add comments explaining what each pattern matches
  - Add tests for each regex
  - Consider `sync.Once` for lazy compilation if needed

### Testing
- [ ] **Add benchmark tests**
  - Cache operations performance
  - Text normalization speed
  - Similarity calculation efficiency
  - Response parsing benchmarks
  - Establish performance baselines

### Performance
- [ ] **Optimize string operations**
  - Use `strings.Builder` in hot paths
  - Reduce allocations in normalization functions
  - Minor performance improvement under load

### Configuration
- [ ] **Add graceful shutdown** (main.go:2767-2787)
  - Handle SIGTERM/SIGINT signals
  - Graceful HTTP server shutdown
  - Context cancellation for goroutines
  - Wait for in-flight requests

### Security
- [ ] **Add panic recovery** (throughout main.go)
  - Currently only in message handler (lines 2644-2651)
  - Add to all goroutines
  - **Risk**: Other goroutines could crash entire bot

### Features
- [ ] **Add search confidence indicators**
  - Show when match is approximate vs exact
  - Display similarity score threshold
  - Help users identify potential mismatches

- [ ] **Add basic metrics/analytics**
  - Request counts by platform
  - Cache hit/miss rates
  - Search success rates
  - Response time tracking
  - Use structured logging with zap

### Dependencies
- [ ] **Add dependency management**
  - Setup Dependabot for automated updates
  - Pin dependency versions explicitly
  - Regular security audit of dependencies

---

## =� Implementation Phases

### Phase 1: Security Hardening (Priority: Critical + High)
Focus on rate limiting, input validation, service hardening

### Phase 2: Code Quality & Testing (Priority: High + Medium)
Refactor complex functions, add comprehensive tests, improve error handling

### Phase 3: Performance & Reliability (Priority: Medium)
Fix concurrency issues, optimize cache, add request deduplication

### Phase 4: Documentation & DevOps (Priority: Medium)
Expand README, add CI/CD, improve configuration

### Phase 5: Polish & Features (Priority: Low)
Metrics, graceful shutdown, UX improvements, benchmarks

---

**Total Items**: 40+ improvements across all priorities
**Estimated Effort**: 12-16 hours for complete implementation
**Architecture**: Staying with monolith (single main.go file)

---

## =� Notes

- `.env` file is properly gitignored and local-only (not exposed)
- Keeping monolith architecture as requested
- All improvements maintain single-file structure
- Focus on production-readiness: security, reliability, observability
