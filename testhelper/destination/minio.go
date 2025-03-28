package destination

import (
	_ "encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	_ "github.com/lib/pq"
	"github.com/minio/minio-go"
	"github.com/ory/dockertest/v3"
	dc "github.com/ory/dockertest/v3/docker"
	"github.com/phayes/freeport"
)

type MINIOResource struct {
	Endpoint   string
	BucketName string
	Port       string
	AccessKey  string
	SecretKey  string
	SiteRegion string
	Client     *minio.Client
}

func SetupMINIO(pool *dockertest.Pool, d cleaner) (*MINIOResource, error) {
	minioPortInt, err := freeport.GetFreePort()
	if err != nil {
		fmt.Println(err)
	}
	minioPort := fmt.Sprintf("%s/tcp", strconv.Itoa(minioPortInt))
	log.Println("minioPort:", minioPort)
	// Setup MINIO
	var minioClient *minio.Client

	options := &dockertest.RunOptions{
		Repository: "minio/minio",
		Tag:        "latest",
		Cmd:        []string{"server", "/data"},
		PortBindings: map[dc.Port][]dc.PortBinding{
			"9000/tcp": {{HostPort: strconv.Itoa(minioPortInt)}},
		},
		Env: []string{
			"MINIO_ACCESS_KEY=MYACCESSKEY",
			"MINIO_SECRET_KEY=MYSECRETKEY",
			"MINIO_SITE_REGION=us-east-1",
		},
	}

	minioContainer, err := pool.RunWithOptions(options)
	if err != nil {
		return nil, err
	}
	d.Cleanup(func() {
		if err := pool.Purge(minioContainer); err != nil {
			d.Log("Could not purge resource:", err)
		}
	})

	minioEndpoint := fmt.Sprintf("localhost:%s", minioContainer.GetPort("9000/tcp"))

	// exponential backoff-retry, because the application in the container might not be ready to accept connections yet
	// the minio client does not do service discovery for you (i.e. it does not check if connection can be established), so we have to use the health check
	if err := pool.Retry(func() error {
		url := fmt.Sprintf("http://%s/minio/health/live", minioEndpoint)
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status code not OK")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// now we can instantiate minio client
	minioClient, err = minio.New(minioEndpoint, "MYACCESSKEY", "MYSECRETKEY", false)
	if err != nil {
		return nil, err
	}
	// Create bucket for MINIO
	// Create a bucket at region 'us-east-1' with object locking enabled.
	minioBucketName := "devintegrationtest"
	err = minioClient.MakeBucket(minioBucketName, "us-east-1")
	if err != nil {
		return nil, err
	}
	return &MINIOResource{
		Endpoint:   minioEndpoint,
		BucketName: minioBucketName,
		Port:       minioContainer.GetPort("9000/tcp"),
		AccessKey:  "MYACCESSKEY",
		SecretKey:  "MYSECRETKEY",
		SiteRegion: "us-east-1",
		Client:     minioClient,
	}, nil
}
