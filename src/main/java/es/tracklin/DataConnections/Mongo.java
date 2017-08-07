package es.tracklin.DataConnections;

import com.mongodb.BasicDBObject;
import com.mongodb.DBCollection;
import com.mongodb.MongoClient;
import com.mongodb.WriteResult;
import com.mongodb.client.MongoCollection;
import com.mongodb.client.MongoDatabase;
import es.tracklin.Client.ClientData;
import es.tracklin.Configuration.MongoConfiguration;
import org.bson.Document;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Configuration;
import org.springframework.stereotype.Component;

import java.util.Date;

//import static java.util.concurrent.TimeUnit.SECONDS;

@Component
@Configuration
public class Mongo {
    @Autowired
    private MongoConfiguration mongoConfiguration;

    public MongoCollection<Document>  connectMongo(String table) {
        String address = mongoConfiguration.getAddress();
        int port = mongoConfiguration.getPort();

        MongoClient mongoClient = new MongoClient(mongoConfiguration.getAddress(), mongoConfiguration.getPort());
        MongoDatabase mongoDatabase = mongoClient.getDatabase("tracklines");

        return mongoDatabase.getCollection(table);
    }

    public String addUser(ClientData clientData) {
        MongoCollection<Document> mongoDatabase = connectMongo("user");

        Document doc = new Document();
        doc.put("name", clientData.getName());
        doc.put("password", clientData.getPassword());
        doc.put("email", clientData.getContactDetails().getEmail());
        doc.put("contactName", clientData.getContactDetails().getName());
        doc.put("contactNumber", clientData.getContactDetails().getNumber());
        doc.put("tokenInterface", clientData.getTokens().getInterface());
        doc.put("tokenAPI", clientData.getTokens().getAPI());
        doc.put("createdDate", new Date());

        mongoDatabase.insertOne(doc);

        return "";
    }
}
