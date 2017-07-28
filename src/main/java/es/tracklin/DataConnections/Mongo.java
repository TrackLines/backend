package es.tracklin.DataConnections;

import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.beans.factory.annotation.Autowired;

public class Mongo {
    @Autowired
    MongoConfiguration mongoConfiguration;

    public String connectMongo() {
        String address = mongoConfiguration.getAddress();
        int port = mongoConfiguration.getPort();

        return address;
    }
}
