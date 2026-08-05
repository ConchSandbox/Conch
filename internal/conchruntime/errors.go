package conchruntime

import "fmt"

// TemplateNotFoundError identifies an absent Template independently of its store.
type TemplateNotFoundError struct {
	ID string
}

func (e *TemplateNotFoundError) Error() string {
	return fmt.Sprintf("template %q not found", e.ID)
}
