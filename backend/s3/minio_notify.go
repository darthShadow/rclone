package s3

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rclone/rclone/fs"
)

type (
	MinioMQTTMessage struct {
		EventName string `json:"EventName"`
		Key       string `json:"Key"`
		Records   []struct {
			EventVersion string    `json:"eventVersion"`
			EventSource  string    `json:"eventSource"`
			AwsRegion    string    `json:"awsRegion"`
			EventTime    time.Time `json:"eventTime"`
			EventName    string    `json:"eventName"`
			UserIdentity struct {
				PrincipalID string `json:"principalId"`
			} `json:"userIdentity"`
			RequestParameters struct {
				AccessKey       string `json:"accessKey"`
				Region          string `json:"region"`
				SourceIPAddress string `json:"sourceIPAddress"`
			} `json:"requestParameters"`
			ResponseElements struct {
				ContentLength        string `json:"content-length"`
				XAmzRequestID        string `json:"x-amz-request-id"`
				XMinioDeploymentID   string `json:"x-minio-deployment-id"`
				XMinioOriginEndpoint string `json:"x-minio-origin-endpoint"`
			} `json:"responseElements"`
			S3 struct {
				S3SchemaVersion string `json:"s3SchemaVersion"`
				ConfigurationID string `json:"configurationId"`
				Bucket          struct {
					Name          string `json:"name"`
					OwnerIdentity struct {
						PrincipalID string `json:"principalId"`
					} `json:"ownerIdentity"`
					Arn string `json:"arn"`
				} `json:"bucket"`
				Object struct {
					Key          string `json:"key"`
					Size         int    `json:"size"`
					ETag         string `json:"eTag"`
					ContentType  string `json:"contentType"`
					UserMetadata struct {
						XAmzMetaMd5Chksum        string `json:"X-Amz-Meta-Md5chksum"`
						XAmzMetaMtime            string `json:"X-Amz-Meta-Mtime"`
						XMinioInternalActualSize string `json:"X-Minio-Internal-actual-size"`
						ContentType              string `json:"content-type"`
						XAmzStorageClass         string `json:"x-amz-storage-class"`
					} `json:"userMetadata"`
					Sequencer string `json:"sequencer"`
				} `json:"object"`
			} `json:"s3"`
			Source struct {
				Host      string `json:"host"`
				Port      string `json:"port"`
				UserAgent string `json:"userAgent"`
			} `json:"source"`
		} `json:"Records"`
	}
)

func (f *Fs) minioChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	var (
		listenerContext       context.Context
		listenerContextCancel context.CancelFunc
	)
	for {
		select {
		case pollInterval, ok := <-pollIntervalChan:
			if !ok {
				if listenerContextCancel != nil {
					listenerContextCancel()
				}
				return
			}
			if pollInterval != 0 {
				if listenerContextCancel != nil {
					listenerContextCancel()
				}
				listenerContext, listenerContextCancel = context.WithCancel(ctx)
				//go f.minioListenAPINotifications(listenerContext, notifyFunc)
				go f.minioListenMQTTNotifications(listenerContext, notifyFunc)
			}
		}
	}
}

func (f *Fs) minioListenAPINotifications(ctx context.Context, notifyFunc func(string,
	fs.EntryType)) {

	httpClient := getClient(ctx, &(f.opt))
	minioClient, err := minio.New(f.opt.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(f.opt.AccessKeyID, f.opt.SecretAccessKey, f.opt.SessionToken),
		Secure:    true,
		Transport: httpClient.Transport,
		Region:    f.opt.Region,
	})
	if err != nil {
		fs.Errorf(f, "Failed to configure minio API client: %v", err)
		return
	}
	fs.Infof(f, "Listening to Minio API Notifications: %s, %s", f.rootBucket, f.rootDirectory)
	for notificationInfo := range minioClient.ListenBucketNotification(ctx,
		f.rootBucket, f.rootDirectory, "", []string{
			"s3:ObjectCreated:*",
			"s3:ObjectRemoved:*",
		}) {
		if notificationInfo.Err != nil {
			fs.Errorf(f, "Minio API notification error: %v", notificationInfo.Err)
		}
		for _, notificationEvent := range notificationInfo.Records {
			change := notificationEvent.EventName
			changedBucket := notificationEvent.S3.Bucket.Name
			changedObject := notificationEvent.S3.Object.Key
			changedPath := strings.Join([]string{changedBucket, changedObject}, "/")
			fs.Infof(f, "Received Minio API Notification: %s, %s", change, changedPath)
			notifyFunc(changedPath, fs.EntryObject)
		}
	}
}

func (f *Fs) minioListenMQTTNotifications(ctx context.Context, notifyFunc func(string,
	fs.EntryType)) {

	var messageHandler mqtt.MessageHandler = func(_ mqtt.Client, message mqtt.Message) {
		fs.Infof(f, "Received Message: %s, %s", message.Topic(), message.Payload())
		var notificationInfo MinioMQTTMessage
		if err := json.Unmarshal(message.Payload(), &notificationInfo); err != nil {
			fs.Errorf(f, "Minio MQTT notification error: %v", err)
			return
		}
		for _, notificationEvent := range notificationInfo.Records {
			change, _ := url.QueryUnescape(notificationEvent.EventName)
			changedBucket, _ := url.QueryUnescape(notificationEvent.S3.Bucket.Name)
			changedObject, _ := url.QueryUnescape(notificationEvent.S3.Object.Key)
			changedPath := strings.Join([]string{changedBucket, changedObject}, "/")
			fs.Infof(f, "Received Minio MQTT Notification: %s, %s", change, changedPath)
			notifyFunc(changedPath, fs.EntryObject)
		}
	}

	//create a ClientOptions struct setting the broker address, client-id, username,
	//password & the on-connect handler
	opts := mqtt.NewClientOptions()
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(false)
	opts.AddBroker("wss://")
	opts.SetUsername("")
	opts.SetPassword("")
	opts.SetClientID(f.ci.UserAgent)

	//create and start a client using the above ClientOptions
	mqttConnection := mqtt.NewClient(opts)
	if token := mqttConnection.Connect(); token.Wait() && token.Error() != nil {
		fs.Errorf(f, "Failed to configure minio MQTT client: %v", token.Error())
		return
	}

	//subscribe to the topic and request messages to be delivered
	//at the specified maximum qos, wait for the receipt to confirm the subscription
	if token := mqttConnection.Subscribe("minio", 1,
		messageHandler); token.Wait() && token.Error() != nil {

		fs.Errorf(f, "Failed to subscribe to minio MQTT topic: %v", token.Error())
		return
	}

	fs.Infof(f, "Listening to Minio MQTT Notifications: %s, %s", f.rootBucket, f.rootDirectory)

	for {
		select {
		case <-ctx.Done():
			mqttConnection.Disconnect(10)
			return
		}
	}
}
