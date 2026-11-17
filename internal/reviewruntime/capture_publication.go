package reviewruntime

// publicationCustody protects a writer through staging removal, not just open
// descriptors. Non-Linux stores keep their existing unleased legacy behavior.
type publicationCustody interface {
	Validate() error
	Close() error
}
