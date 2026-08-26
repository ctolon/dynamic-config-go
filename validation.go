package dynamicconfig

// Validator inspects a freshly decoded configuration and reports whether it
// may be published.
//
// The signature is deliberately the whole of the validation contract. The
// package does not depend on a validation library and does not reflect over
// struct tags, so go-playground/validator, ozzo-validation, generated code
// or a handful of if statements are all equally first-class:
//
//	func validate(c *AppConfig) error {
//	    if c.Server.Port < 1 || c.Server.Port > 65535 {
//	        return fmt.Errorf("server.port %d out of range", c.Server.Port)
//	    }
//
//	    return nil
//	}
//
// A validator must not retain or mutate the value it is given, and must not
// call back into the Config it validates for — it runs inside the reload
// transaction, and a Reload from within a validator would deadlock until
// the caller's context expires.
//
// An error returned here rejects the candidate. The previously published
// snapshot stays current: that is the last-known-good guarantee, and the
// validator is what gives it teeth.
type Validator[T any] func(*T) error
