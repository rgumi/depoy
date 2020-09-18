# Depoy

[![Build Status](https://travis-ci.com/rgumi/depoy.svg?branch=master)](https://travis-ci.com/rgumi/depoy)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=rgumi_deploy&metric=alert_status)](https://sonarcloud.io/dashboard?id=rgumi_deploy)

Depoy is an API-Gateway which natively supports Continous Deployment (CD) of RESTful-Application. It evaluates the state of an upstream application by collecting HTTP-Connection metrics and by scraping the Prometheus-Endpoint of the upstream application - if provided. It integrates into Prometheus and offers a reactive web-application for configuration and monitoring.

![API-Gateway functionality](images/APIGAtewayOverview.png "API-Gateway functionality")


## Architecture

The API-Gateway is built using Go for all backend tasks and Vue for the web-application. 

![Depoy components](images/OverviewDiagram.png "Depoy Components")