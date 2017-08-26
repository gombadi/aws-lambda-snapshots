package main

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

type ami struct {
	svc     *ec2.EC2
	tagname string // the tagname to mark AMI's
}

type faBkupInstance struct {
	ii *ec2.CreateImageInput // details of an instance tht needs to be backed up
	e  error                 // any errors while getting this info
}

// Run function is the function called by the cli library to run the actual sub command code.
func (a *ami) amiCreate(amiCreateChan chan *workResults) {

	// instChan is the input channel for the system
	instChan := make(chan *ec2.CreateImageInput)
	resChan := make(chan *workResult) // results and output chan
	doneChan := make(chan *workResults)

	// Create an EC2 service object
	// config values keys, sercet key & region read from environment
	//a.svc = ec2.New(a.sess, &aws.Config{MaxRetries: aws.Int(10)})
	var wg sync.WaitGroup

	// create 3 workers that will create the AMI's
	// ami create takes a lot of work so don't overload the system and
	// hit the AWS rate limiters
	for x := 0; x <= 3; x++ {
		wg.Add(1)
		go a.createandTagImage(instChan, resChan, &wg)
	}

	// create a channel to receive instances needing bkup
	faInstChan := getBkupInstances(a.svc, a.tagname)

	// start the results goroutine running
	go a.processResults(resChan, doneChan)

	//  start pushing things into the system
	for faBkupInst := range faInstChan {
		if faBkupInst.e != nil {
			fmt.Printf("error received getting bkup instance details: %v\n", faBkupInst.e)
			continue
		}
		instChan <- faBkupInst.ii
	}

	// all instances to be backed up have been pushed to the workers so close their input channel
	close(instChan)

	// wait for the goroutines to do their work and exit
	wg.Wait()

	// Now we know the workers are gone and therefore no more work will be sent on the resChan
	// this will exit the results processor range over the resChan
	close(resChan)
	// wait for the results goroutine to shutdown then return
	wrs := <-doneChan
	close(doneChan)

	// update complete time
	wrs.endtime = time.Now().String()
	// send on the chan to indicate we are done
	amiCreateChan <- wrs
	return
}

func (a *ami) createandTagImage(instChan chan *ec2.CreateImageInput, resChan chan *workResult, wg *sync.WaitGroup) {

	defer wg.Done()

	// now we have the slice of instanceIds to be backed up we can create the AMI then tag them
	for abkupInstance := range instChan {

		// snapshot the instance to create the ami
		createImageResp, err := a.svc.CreateImage(abkupInstance)

		if err != nil {
			resChan <- &workResult{err: fmt.Errorf("error creating AMI for instance %s err: %v\n", *abkupInstance.InstanceId, err)}
			continue
		}

		err = a.tagImage(*createImageResp.ImageId)
		if err != nil {
			resChan <- &workResult{err: fmt.Errorf("error tagging AMI fot instance %s err: %v\n", *abkupInstance.InstanceId, err)}
		}
		resChan <- &workResult{message: fmt.Sprintf("completed snapshot of instance: %s ami: %s\n", *abkupInstance.InstanceId, *createImageResp.ImageId)}
	}
}

func (a *ami) tagImage(amiId string) error {

	// Note - can not use wait image available as that waits until the full ami creation is done
	// while we only want to wait until the ami exists and can therefore be tagged

	var err error

	// check if the image exists
	params := &ec2.DescribeImagesInput{
		ImageIds: []*string{
			aws.String(amiId),
		},
	}

	retryCount := 0
	for retryCount < 25 {
		time.Sleep(time.Millisecond * 1987)

		_, err = a.svc.DescribeImages(params)
		// if no error then image exists and can be tagged
		if err == nil {
			retryCount = 99
		}
		// off around the loop again
		retryCount++
	}

	ec2cti := &ec2.CreateTagsInput{
		Resources: []*string{aws.String(amiId)},
		Tags: []*ec2.Tag{
			&ec2.Tag{
				Key:   aws.String(a.tagname),
				Value: aws.String(strconv.FormatInt(time.Now().Unix(), 10))}},
	}

	// call the create tag func
	_, err = a.svc.CreateTags(ec2cti)
	return err // which may be nil
}

// processResults runs in a goroutine and reads results from the workers and updates a global map
func (a *ami) processResults(resChan chan *workResult, doneChan chan *workResults) {

	var ebb, mbb bytes.Buffer

	for r := range resChan {
		if r.err != nil {
			ebb.WriteString(fmt.Sprintf("%v", r.err))
		}
		if r.message != "" {
			mbb.WriteString(r.message)
		}
	}
	// tell the world we are done
	doneChan <- &workResults{
		messages: mbb,
		errs:     ebb,
	}
}

// getBkupInstances will return a channel of future answers containing
// details of instances that need to be backed up.
func getBkupInstances(svc *ec2.EC2, tagname string) chan *faBkupInstance {

	c := make(chan *faBkupInstance)

	go func(c chan *faBkupInstance, tn string) {
		defer close(c)

		params := &ec2.DescribeInstancesInput{
			Filters: []*ec2.Filter{
				{
					Name: aws.String("tag-key"),
					Values: []*string{
						aws.String(tn),
					},
				},
			},
		}

		resp, err := svc.DescribeInstances(params)

		if err != nil {
			c <- &faBkupInstance{ii: nil, e: err}
			return
		}

		// for any instance found extract tag name and instanceid
		for reservation := range resp.Reservations {
			for _, inst := range resp.Reservations[reservation].Instances {
				// Create a new theInstance variable for each run through the loop
				newImage := &ec2.CreateImageInput{}
				newImage.NoReboot = aws.Bool(true)

				for _, t := range inst.Tags {
					switch *t.Key {
					case "Name":
						newImage.Name = aws.String(
							*t.Value + "-" + strconv.FormatInt(time.Now().Unix(), 10))
					case "bkupReboot":
						// value in tag is reboot so if tag not present default is no reboot
						if b, err := strconv.ParseBool(*t.Value); err == nil {
							// swap value as the question is NoReboot?
							newImage.NoReboot = aws.Bool(!b)
						}
					default:
					}
				}
				if *newImage.Name == "" {
					newImage.Name = aws.String(
						*inst.InstanceId + "-" + strconv.FormatInt(time.Now().Unix(), 10))
				}
				newImage.Description = aws.String("Auto backup of instance " + *inst.InstanceId)
				newImage.InstanceId = inst.InstanceId
				// append details on this instance to the slice
				//fmt.Printf("found new: %s\n", *newImage.InstanceId)
				c <- &faBkupInstance{ii: newImage, e: nil}
			}
		}
	}(c, tagname)
	return c
}

/*
 */
