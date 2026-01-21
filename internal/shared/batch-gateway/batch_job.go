
package batch_shared
// Job represents a processing job.
type Job struct {
    RequetId    string
    Model string
    ID          string
    SLO string
    EndPoint    string
    RequestBody  []byte
    //... other job fields
}
