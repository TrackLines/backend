package es.tracklin.DataConnections;

import com.mongodb.MongoClient;
import com.mongodb.client.MongoDatabase;
import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.context.annotation.Configuration;

//import static java.util.concurrent.TimeUnit.SECONDS;

@Configuration
public class Mongo {
    private MongoConfiguration mongoConfiguration;

    public Mongo() {
        this.mongoConfiguration = new MongoConfiguration();
    }

    public MongoDatabase connectMongo() {
        String address = mongoConfiguration.getAddress();
        int port = mongoConfiguration.getPort();

        MongoClient mongoClient = new MongoClient(mongoConfiguration.getAddress(), mongoConfiguration.getPort());

        return mongoClient.getDatabase(mongoConfiguration.getDatabase());
    }
}
