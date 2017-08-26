# aws-lambda-snapshots
Backup AWS EC2 instances using Lambda functions

## Purpose

This code runs in as a cron based Lambda function and will create an AMI for each tagged ec2 instance in your account.

For full details of the code and usage see [this blog entry](https://www.gombadi.com/post/aws-lambda-bkups/)

## Install

Download this repo into your Go Path and create a base Lambda function giving it a name. Update the buildme.sh script to use the correct Lambda function name then run the script. The script will compile the code for Linux, create a zip file containing the Node.js wrapper and then upload it to the Lambda function in your account.

Set up a cron entry to trigger the Lambda function when you want it to run - Note Lambda crons use UTC as the time base.

Add support for github SNS services - in release branch

