package rpc

import (
	"crypto/x509"
	"io/ioutil"

	"github.com/gravitational/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config specifies rpc config.
type Config struct {
	// CAFile specifies the path to TLS Certificate Authority file
	CAFile string
	// CertFile specifies the path to TLS certificate file
	CertFile string
	// KeyFile specifies the path to TLS certificate key file
	KeyFile string
}

// CheckAndSetDefaults validates this configuration object.
// Configvalues that were not specified will be set to their default values if
// available.
func (r *Config) CheckAndSetDefaults() error {
	var errors []error
	if r.CAFile == "" {
		errors = append(errors, trace.BadParameter("certificate authority file must be provided"))
	}
	if r.CertFile == "" {
		errors = append(errors, trace.BadParameter("certificate must be provided"))
	}
	if r.KeyFile == "" {
		errors = append(errors, trace.BadParameter("certificate key must be provided"))
	}
	return trace.NewAggregate(errors...)
}

// NewGRPCServer constructs a new GRPC server with the provided config.
func NewGRPCServer(config Config) (*grpc.Server, error) {
	creds, err := credentials.NewServerTLSFromFile(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, trace.Wrap(err, "failed to read certificate/key from %v/%v", config.CertFile, config.KeyFile)
	}

	caCert, err := ioutil.ReadFile(config.CAFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	return grpc.NewServer(grpc.Creds(creds)), nil
}
