package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

type amidelete struct {
	svc      *ec2.EC2
	tagname  string // the tagname to mark AMI's
	keepDays string // number of days to keep the AMI
}

type faImage struct {
	id string   // image id to be deregistered
	ss []string // slice of snapshots to be deleted
	e  error    // any errors while getting this info
}

// Run function is the function called by the cli library to run the actual sub command code.
func (a *amidelete) amiDelete(amiDeleteChan chan *workResults) {

	// instChan is the input channel for the system
	imageChan := make(chan *faImage)
	resChan := make(chan *workResult) // results and output chan
	doneChan := make(chan *workResults)

	// Create an EC2 service object
	// config values keys, sercet key & region read from environment
	var wg sync.WaitGroup

	// create a channel to receive instances needing bkup
	faImageChan := getOldImages(a.svc, a.tagname, a.keepDays)

	// create 3 workers that will deregister the AMI's
	// don't overload the system and hit the AWS rate limiters
	for x := 0; x <= 3; x++ {
		wg.Add(1)
		go a.deregisterandDelete(imageChan, resChan, &wg)
	}

	// start the results goroutine running
	go a.processResults(resChan, doneChan)

	//  start pushing things into the system
	for faImage := range faImageChan {
		if faImage.e != nil {
			fmt.Printf("error received getting bkup instance details: %v\n", faImage.e)
			continue
		}
		imageChan <- faImage
	}

	// all instances to be backed up have been pushed to the workers so close their input channel
	close(imageChan)

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
	amiDeleteChan <- wrs
	return // and goroutine ends
}

func (a *amidelete) deregisterandDelete(imageChan chan *faImage, resChan chan *workResult, wg *sync.WaitGroup) {

	defer wg.Done()
	var err error
	var se string

	// now we have the slice of instanceIds to be backed up we can create the AMI then tag them
	for afaImage := range imageChan {
		se = ""

		ec2dii := &ec2.DeregisterImageInput{
			ImageId: aws.String(afaImage.id),
		}

		_, err = a.svc.DeregisterImage(ec2dii)
		if err != nil {
			resChan <- &workResult{err: fmt.Errorf("error deregistering AMI: %v\n", err)}
			continue
		}

		// now work through all the associated snapshots
		for _, ss := range afaImage.ss {
			err = a.deleteSnapshot(ss)
			if err != nil {
				resChan <- &workResult{err: err}
				se = "(with errors)"
			}
		}
		resChan <- &workResult{message: fmt.Sprintf("completed deregistering AMI%s: %s snapshots: %s\n", se, afaImage.id, strings.Join(afaImage.ss, ","))}
	}
}

func (a *amidelete) deleteSnapshot(snapshot string) error {

	var err error

	ec2dsi := &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(snapshot),
	}

	retryCount := 0
	for retryCount < 10 {
		_, err = a.svc.DeleteSnapshot(ec2dsi)

		// if no error then snapshot deleted so return
		if err == nil {
			// deleted this snapshot so on to the next
			return nil
		}
		// off around the loop again
		retryCount++

		time.Sleep(time.Millisecond * 1987)
	}
	return fmt.Errorf("error - unable to delete snapshot within retry count. snapshot: %s last error: %v\n", snapshot, err)
}

// processResults runs in a goroutine and reads results from the workers and updates a global map
func (a *amidelete) processResults(resChan chan *workResult, doneChan chan *workResults) {

	var ebb, mbb bytes.Buffer

	// read things from the resChan until it is closed
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
func getOldImages(svc *ec2.EC2, tagname, keepDays string) chan *faImage {

	c := make(chan *faImage)

	go func(c chan *faImage, tn, keepDays string) {
		defer close(c)

		ec2dii := &ec2.DescribeImagesInput{
			Owners: []*string{aws.String("self")},
			Filters: []*ec2.Filter{
				&ec2.Filter{
					Name:   aws.String("tag-key"),
					Values: []*string{aws.String(tn)},
				},
			},
		}

		imagesResp, err := svc.DescribeImages(ec2dii)
		if err != nil {
			c <- &faImage{e: err}
			return
		}

		// sanity check to make sure we don't remove all images from account
		if len(imagesResp.Images) == 0 {
			return // which will defer close the chan indicating there are no ami's to delete
		}

		// for each image that is to be removed
		for _, image := range imagesResp.Images {
			for _, tag := range image.Tags {
				if *tag.Key == tn {
					// check if time is up for this AMI

					if checkExpired(tag, keepDays) {
						// push this image into the channel to be deregistered
						c <- &faImage{e: nil, id: *image.ImageId, ss: getSnapshots(image)}
					}
				}
			}
		}
		// end goroutine which will defer close the input channel
	}(c, tagname, keepDays)
	return c
}

func getSnapshots(image *ec2.Image) []string {
	var ss []string

	for _, blockDM := range image.BlockDeviceMappings {
		// some block devices are not on EBS
		if blockDM.Ebs == nil {
			continue
		}
		if len(*blockDM.Ebs.SnapshotId) > 0 {
			ss = append(ss, *blockDM.Ebs.SnapshotId)
		}
	}
	return ss
}

func checkExpired(t *ec2.Tag, keepDays string) bool {

	// if ami more than this number of days old then remove it
	// FIXME - should be a config item somewhere

	keepAMIFor, err := strconv.Atoi(keepDays)
	if err != nil {
		keepAMIFor = 3
	}

	// extract the time this AMI was created
	amiCreation, _ := strconv.ParseInt(*t.Value, 10, 64)
	amiLifeSpan := time.Now().Unix() - amiCreation

	if int64(keepAMIFor*86000) < amiLifeSpan {
		return true
	}
	return false
}

/*
One last change to test
*/
