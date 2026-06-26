package llm

import "fmt"

type ProviderErrorClass string

const (
	ProviderErrorAuth         ProviderErrorClass = "auth"
	ProviderErrorRateLimit    ProviderErrorClass = "rate_limit"
	ProviderErrorServer       ProviderErrorClass = "server"
	ProviderErrorNetwork      ProviderErrorClass = "network"
	ProviderErrorDecode       ProviderErrorClass = "decode"
	ProviderErrorContextLimit ProviderErrorClass = "context_limit"
	ProviderErrorUnknown      ProviderErrorClass = "unknown"
)

type ProviderError struct {
	Class        ProviderErrorClass
	statusCode   int
	providerHost string
	detail       string
	retryable    bool
}

func NewProviderError(class ProviderErrorClass, statusCode int, providerHost, detail string, retryable bool) *ProviderError {
	if class == "" {
		class = ProviderErrorUnknown
	}
	return &ProviderError{
		Class:        class,
		statusCode:   statusCode,
		providerHost: providerHost,
		detail:       detail,
		retryable:    retryable,
	}
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	if e.statusCode > 0 {
		return fmt.Sprintf("llm http %d: %s", e.statusCode, e.detail)
	}
	if e.providerHost != "" {
		return fmt.Sprintf("llm %s error from %s: %s", e.Class, e.providerHost, e.detail)
	}
	return fmt.Sprintf("llm %s error: %s", e.Class, e.detail)
}

func (e *ProviderError) Temporary() bool {
	return e != nil && e.retryable
}

func (e *ProviderError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func (e *ProviderError) ProviderHost() string {
	if e == nil {
		return ""
	}
	return e.providerHost
}

func (e *ProviderError) SafeDetail() string {
	if e == nil {
		return ""
	}
	return e.detail
}
