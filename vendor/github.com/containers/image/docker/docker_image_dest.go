package docker

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Sirupsen/logrus"
	"github.com/containers/image/docker/reference"
	"github.com/containers/image/manifest"
	"github.com/containers/image/types"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

var manifestMIMETypes = []string{
	// TODO(runcom): we'll add OCI as part of another PR here
	manifest.DockerV2Schema2MediaType,
	manifest.DockerV2Schema1SignedMediaType,
	manifest.DockerV2Schema1MediaType,
}

func supportedManifestMIMETypesMap() map[string]bool {
	m := make(map[string]bool, len(manifestMIMETypes))
	for _, mt := range manifestMIMETypes {
		m[mt] = true
	}
	return m
}

type dockerImageDestination struct {
	ref dockerReference
	c   *dockerClient
	// State
	manifestDigest digest.Digest // or "" if not yet known.
}

// newImageDestination creates a new ImageDestination for the specified image reference.
func newImageDestination(ctx *types.SystemContext, ref dockerReference) (types.ImageDestination, error) {
	c, err := newDockerClient(ctx, ref, true, "push")
	if err != nil {
		return nil, err
	}
	return &dockerImageDestination{
		ref: ref,
		c:   c,
	}, nil
}

// Reference returns the reference used to set up this destination.  Note that this should directly correspond to user's intent,
// e.g. it should use the public hostname instead of the result of resolving CNAMEs or following redirects.
func (d *dockerImageDestination) Reference() types.ImageReference {
	return d.ref
}

// Close removes resources associated with an initialized ImageDestination, if any.
func (d *dockerImageDestination) Close() error {
	return nil
}

func (d *dockerImageDestination) SupportedManifestMIMETypes() []string {
	return manifestMIMETypes
}

// SupportsSignatures returns an error (to be displayed to the user) if the destination certainly can't store signatures.
// Note: It is still possible for PutSignatures to fail if SupportsSignatures returns nil.
func (d *dockerImageDestination) SupportsSignatures() error {
	return errors.Errorf("Pushing signatures to a Docker Registry is not supported")
}

// ShouldCompressLayers returns true iff it is desirable to compress layer blobs written to this destination.
func (d *dockerImageDestination) ShouldCompressLayers() bool {
	return true
}

// AcceptsForeignLayerURLs returns false iff foreign layers in manifest should be actually
// uploaded to the image destination, true otherwise.
func (d *dockerImageDestination) AcceptsForeignLayerURLs() bool {
	return true
}

// sizeCounter is an io.Writer which only counts the total size of its input.
type sizeCounter struct{ size int64 }

func (c *sizeCounter) Write(p []byte) (n int, err error) {
	c.size += int64(len(p))
	return len(p), nil
}

// PutBlob writes contents of stream and returns data representing the result (with all data filled in).
// inputInfo.Digest can be optionally provided if known; it is not mandatory for the implementation to verify it.
// inputInfo.Size is the expected length of stream, if known.
// WARNING: The contents of stream are being verified on the fly.  Until stream.Read() returns io.EOF, the contents of the data SHOULD NOT be available
// to any other readers for download using the supplied digest.
// If stream.Read() at any time, ESPECIALLY at end of input, returns an error, PutBlob MUST 1) fail, and 2) delete any data stored so far.
func (d *dockerImageDestination) PutBlob(stream io.Reader, inputInfo types.BlobInfo) (types.BlobInfo, error) {
	if inputInfo.Digest.String() != "" {
		checkURL := fmt.Sprintf(blobsURL, reference.Path(d.ref.ref), inputInfo.Digest.String())

		logrus.Debugf("Checking %s", checkURL)
		res, err := d.c.makeRequest("HEAD", checkURL, nil, nil)
		if err != nil {
			return types.BlobInfo{}, err
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
			logrus.Debugf("... already exists, not uploading")
			return types.BlobInfo{Digest: inputInfo.Digest, Size: getBlobSize(res)}, nil
		case http.StatusUnauthorized:
			logrus.Debugf("... not authorized")
			return types.BlobInfo{}, errors.Errorf("not authorized to read from destination repository %s", reference.Path(d.ref.ref))
		case http.StatusNotFound:
			// noop
		default:
			return types.BlobInfo{}, errors.Errorf("failed to read from destination repository %s: %v", reference.Path(d.ref.ref), http.StatusText(res.StatusCode))
		}
		logrus.Debugf("... failed, status %d", res.StatusCode)
	}

	// FIXME? Chunked upload, progress reporting, etc.
	uploadURL := fmt.Sprintf(blobUploadURL, reference.Path(d.ref.ref))
	logrus.Debugf("Uploading %s", uploadURL)
	res, err := d.c.makeRequest("POST", uploadURL, nil, nil)
	if err != nil {
		return types.BlobInfo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		logrus.Debugf("Error initiating layer upload, response %#v", *res)
		return types.BlobInfo{}, errors.Errorf("Error initiating layer upload to %s, status %d", uploadURL, res.StatusCode)
	}
	uploadLocation, err := res.Location()
	if err != nil {
		return types.BlobInfo{}, errors.Wrap(err, "Error determining upload URL")
	}

	digester := digest.Canonical.Digester()
	sizeCounter := &sizeCounter{}
	tee := io.TeeReader(stream, io.MultiWriter(digester.Hash(), sizeCounter))
	res, err = d.c.makeRequestToResolvedURL("PATCH", uploadLocation.String(), map[string][]string{"Content-Type": {"application/octet-stream"}}, tee, inputInfo.Size, true)
	if err != nil {
		logrus.Debugf("Error uploading layer chunked, response %#v", *res)
		return types.BlobInfo{}, err
	}
	defer res.Body.Close()
	computedDigest := digester.Digest()

	uploadLocation, err = res.Location()
	if err != nil {
		return types.BlobInfo{}, errors.Wrap(err, "Error determining upload URL")
	}

	// FIXME: DELETE uploadLocation on failure

	locationQuery := uploadLocation.Query()
	// TODO: check inputInfo.Digest == computedDigest https://github.com/containers/image/pull/70#discussion_r77646717
	locationQuery.Set("digest", computedDigest.String())
	uploadLocation.RawQuery = locationQuery.Encode()
	res, err = d.c.makeRequestToResolvedURL("PUT", uploadLocation.String(), map[string][]string{"Content-Type": {"application/octet-stream"}}, nil, -1, true)
	if err != nil {
		return types.BlobInfo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		logrus.Debugf("Error uploading layer, response %#v", *res)
		return types.BlobInfo{}, errors.Errorf("Error uploading layer to %s, status %d", uploadLocation, res.StatusCode)
	}

	logrus.Debugf("Upload of layer %s complete", computedDigest)
	return types.BlobInfo{Digest: computedDigest, Size: sizeCounter.size}, nil
}

func (d *dockerImageDestination) HasBlob(info types.BlobInfo) (bool, int64, error) {
	if info.Digest == "" {
		return false, -1, errors.Errorf(`"Can not check for a blob with unknown digest`)
	}
	checkURL := fmt.Sprintf(blobsURL, reference.Path(d.ref.ref), info.Digest.String())

	logrus.Debugf("Checking %s", checkURL)
	res, err := d.c.makeRequest("HEAD", checkURL, nil, nil)
	if err != nil {
		return false, -1, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		logrus.Debugf("... already exists")
		return true, getBlobSize(res), nil
	case http.StatusUnauthorized:
		logrus.Debugf("... not authorized")
		return false, -1, errors.Errorf("not authorized to read from destination repository %s", reference.Path(d.ref.ref))
	case http.StatusNotFound:
		logrus.Debugf("... not present")
		return false, -1, types.ErrBlobNotFound
	default:
		logrus.Errorf("failed to read from destination repository %s: %v", reference.Path(d.ref.ref), http.StatusText(res.StatusCode))
	}
	logrus.Debugf("... failed, status %d, ignoring", res.StatusCode)
	return false, -1, types.ErrBlobNotFound
}

func (d *dockerImageDestination) ReapplyBlob(info types.BlobInfo) (types.BlobInfo, error) {
	return info, nil
}

func (d *dockerImageDestination) PutManifest(m []byte) error {
	digest, err := manifest.Digest(m)
	if err != nil {
		return err
	}
	d.manifestDigest = digest

	refTail, err := d.ref.tagOrDigest()
	if err != nil {
		return err
	}
	url := fmt.Sprintf(manifestURL, reference.Path(d.ref.ref), refTail)

	headers := map[string][]string{}
	mimeType := manifest.GuessMIMEType(m)
	if mimeType != "" {
		headers["Content-Type"] = []string{mimeType}
	}
	res, err := d.c.makeRequest("PUT", url, headers, bytes.NewReader(m))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		body, err := ioutil.ReadAll(res.Body)
		if err == nil {
			logrus.Debugf("Error body %s", string(body))
		}
		logrus.Debugf("Error uploading manifest, status %d, %#v", res.StatusCode, res)
		return errors.Errorf("Error uploading manifest to %s, status %d", url, res.StatusCode)
	}
	return nil
}

func (d *dockerImageDestination) PutSignatures(signatures [][]byte) error {
	// FIXME? This overwrites files one at a time, definitely not atomic.
	// A failure when updating signatures with a reordered copy could lose some of them.

	// Skip dealing with the manifest digest if not necessary.
	if len(signatures) == 0 {
		return nil
	}
	if d.c.signatureBase == nil {
		return errors.Errorf("Pushing signatures to a Docker Registry is not supported, and there is no applicable signature storage configured")
	}

	if d.manifestDigest.String() == "" {
		// This shouldn’t happen, ImageDestination users are required to call PutManifest before PutSignatures
		return errors.Errorf("Unknown manifest digest, can't add signatures")
	}

	for i, signature := range signatures {
		url := signatureStorageURL(d.c.signatureBase, d.manifestDigest, i)
		if url == nil {
			return errors.Errorf("Internal error: signatureStorageURL with non-nil base returned nil")
		}
		err := d.putOneSignature(url, signature)
		if err != nil {
			return err
		}
	}
	// Remove any other signatures, if present.
	// We stop at the first missing signature; if a previous deleting loop aborted
	// prematurely, this may not clean up all of them, but one missing signature
	// is enough for dockerImageSource to stop looking for other signatures, so that
	// is sufficient.
	for i := len(signatures); ; i++ {
		url := signatureStorageURL(d.c.signatureBase, d.manifestDigest, i)
		if url == nil {
			return errors.Errorf("Internal error: signatureStorageURL with non-nil base returned nil")
		}
		missing, err := d.c.deleteOneSignature(url)
		if err != nil {
			return err
		}
		if missing {
			break
		}
	}

	return nil
}

// putOneSignature stores one signature to url.
func (d *dockerImageDestination) putOneSignature(url *url.URL, signature []byte) error {
	switch url.Scheme {
	case "file":
		logrus.Debugf("Writing to %s", url.Path)
		err := os.MkdirAll(filepath.Dir(url.Path), 0755)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(url.Path, signature, 0644)
		if err != nil {
			return err
		}
		return nil

	case "http", "https":
		return errors.Errorf("Writing directly to a %s sigstore %s is not supported. Configure a sigstore-staging: location", url.Scheme, url.String())
	default:
		return errors.Errorf("Unsupported scheme when writing signature to %s", url.String())
	}
}

// deleteOneSignature deletes a signature from url, if it exists.
// If it successfully determines that the signature does not exist, returns (true, nil)
func (c *dockerClient) deleteOneSignature(url *url.URL) (missing bool, err error) {
	switch url.Scheme {
	case "file":
		logrus.Debugf("Deleting %s", url.Path)
		err := os.Remove(url.Path)
		if err != nil && os.IsNotExist(err) {
			return true, nil
		}
		return false, err

	case "http", "https":
		return false, errors.Errorf("Writing directly to a %s sigstore %s is not supported. Configure a sigstore-staging: location", url.Scheme, url.String())
	default:
		return false, errors.Errorf("Unsupported scheme when deleting signature from %s", url.String())
	}
}

// Commit marks the process of storing the image as successful and asks for the image to be persisted.
// WARNING: This does not have any transactional semantics:
// - Uploaded data MAY be visible to others before Commit() is called
// - Uploaded data MAY be removed or MAY remain around if Close() is called without Commit() (i.e. rollback is allowed but not guaranteed)
func (d *dockerImageDestination) Commit() error {
	return nil
}
