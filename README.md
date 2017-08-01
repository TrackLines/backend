# Backend for TrackLin.es

### Travis Build: 
[![Build Status](https://travis-ci.org/TrackLines/backend.svg?branch=master)](https://travis-ci.org/TrackLines/backend)

### Description
This is the backend system for Tracklin.es it is a user tracking system for websites and online stores it will, automatically tell a client when customers have stopped viewing the site and if there are spikes in sales where they came from

### Example
+ User enters site on handbag page
+ Cookie attached to user so that we can follow them through the store
+ User goes to Checkout
+ User leaves without completing checkout

This is automatically flagged as unusual because they didn't complete the purchase, client is informed in analysis page to look into why the sale might not have happened
  
+ User enters site on shes page
+ Cookie attached to user so that we can follow them through the store
+ User goes to Checkout
+ User completes sales

This is automatically flagged as normal activity and nothing else happens

+ 20 users within 1h enters site on cheese page
+ They all complete purchase 
 
This is automatically flagged as unusual traffic and completing from a single page within a short period and client is told that high sales are happening on the cheese product

+ 10 users enters site on hamburger page
+ User goes to Checkout
+ User leaves without completing checkout

This is automatically flagged as unusual and client is informed that large amount of customers are leaving as soon as they get to the checkout page

+ User enters site on shes page
+ Cookie attached to user so that we can follow them through the store
+ User leaves site
+ User returns on a later date
+ User completes sales

This is automatically flagged as return customer and added to analysis for client so that they can see how many users are return customers