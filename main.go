package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sns"
)

// output from a worker
type workResult struct {
	message string // any message the worker wants to pass on
	err     error  // any errors the worker experienced
}

// output from all workers
type workResults struct {
	messages bytes.Buffer // contains any info messages produced
	errs     bytes.Buffer // contains any errors produced
	endtime  string
}

// global config file
var config map[string]string

func main() {

	// load gloabl config file from s3 or defaults
	config := make(map[string]string)
	// adjust the following to suit your system
	config["output"] = "all"
	config["snsTopic"] = "arn:aws:sns:us-west-2:287624545788:NotifyMe"
	config["tagname"] = "lambdabkup"
	config["keepdays"] = "3"

	amiCreateChan := make(chan *workResults)
	amiDeleteChan := make(chan *workResults)

	//svc := ec2.New(sesson.New(), &aws.Config{MaxRetries: aws.Int(10)})
	svc := ec2.New(session.New())

	ac := &ami{
		svc:     svc,
		tagname: config["tagname"],
	}
	ad := &amidelete{
		svc:      svc,
		tagname:  config["tagname"],
		keepDays: config["keepdays"],
	}

	// start the create ami task
	go ac.amiCreate(amiCreateChan)

	// start the ami remove task
	go ad.amiDelete(amiDeleteChan)

	// block and wait for the results
	createWRS := <-amiCreateChan
	deleteWRS := <-amiDeleteChan

	sendOutput := false
	var ob bytes.Buffer

	switch config["output"] {
	case "debug":
		ob.WriteString("#####################Debug config settings\n")
		for k, v := range config {
			ob.WriteString(fmt.Sprintf("config[%s] = %s\n", k, v))
		}
		ob.WriteString("\n")
		fallthrough // after displaying the above continue with the rest
	case "all":
		sendOutput = true
		ob.WriteString("#####################\nOutput:\n")
		if createWRS.messages.Len() > 0 {
			ob.WriteString("Create New AMI's:\n")
			ob.Write(createWRS.messages.Bytes())
		}
		ob.WriteString(fmt.Sprintf("Create AMI task complete: %s\n", createWRS.endtime))
		if deleteWRS.messages.Len() > 0 {
			ob.WriteString("Delete Old AMI's:\n")
			ob.Write(deleteWRS.messages.Bytes())
		}
		ob.WriteString(fmt.Sprintf("Delete AMI task complete: %s\n", deleteWRS.endtime))
		fallthrough // after displaying the above continue with the rest
	case "errors":
		ob.WriteString("\n#####################\nErrors:\n")
		if createWRS.errs.Len() > 0 {
			ob.WriteString("Create New AMI's errors:\n")
			ob.Write(createWRS.errs.Bytes())
			sendOutput = true
		}
		if deleteWRS.errs.Len() > 0 {
			ob.WriteString("Delete Old AMI's errors:\n")
			ob.Write(deleteWRS.errs.Bytes())
			sendOutput = true
		}
	}

	if sendOutput == true {
		if config["snsTopic"] != "" {
			snsparams := &sns.PublishInput{
				Message:  aws.String(ob.String()),                                                     // Report output
				Subject:  aws.String(fmt.Sprintf("Lambda Backup Report for %s", time.Now().String())), // Message Subject for emails
				TopicArn: aws.String(config["snsTopic"]),
			}

			// push the report to SNS for distribution
			snssvc := sns.New(session.New())
			publishResp, err := snssvc.Publish(snsparams)
			if err != nil {
				fmt.Printf("error publishing to AWS SNS: %v\n", err)
				os.Exit(1)
			} else {
				fmt.Printf("Published to AWS SNS: %s\n", *publishResp.MessageId)
			}
		}
	} else {
		fmt.Printf("%s\n", ob.String())
	}
}

/*
 */
