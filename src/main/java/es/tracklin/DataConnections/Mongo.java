package es.tracklin.DataConnections;

import com.mongodb.MongoClient;
import com.mongodb.client.FindIterable;
import com.mongodb.client.MongoCollection;
import com.mongodb.client.MongoDatabase;
import es.tracklin.Client.ClientData;
import es.tracklin.Configuration.MongoConfiguration;
import org.bson.Document;
import org.bson.types.ObjectId;
import org.springframework.context.annotation.Configuration;
import org.springframework.stereotype.Component;
import java.util.Date;

//import static java.util.concurrent.TimeUnit.SECONDS;

@Component
@Configuration
public class Mongo {
    private MongoConfiguration mongoConfiguration;

    public Mongo(MongoConfiguration mongoConfiguration) {
        this.mongoConfiguration = mongoConfiguration;
    }

    private MongoCollection<Document>  connectMongo(String table) {
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

        Object clientObj = doc.get("_id");

        return clientObj.toString();
    }

    public ClientData getUser(String username, String password) {
        Document findDocument = new Document();
        findDocument.put("name", username);
        findDocument.put("password", password);

        ClientData returnData = new ClientData();
        ClientData.Tokens tokens = returnData.new Tokens();
        ClientData.ContactDetails contactDetails = returnData.new ContactDetails();

        MongoCollection<Document> mongoDatabase = connectMongo("user");
        FindIterable<Document> returnDocuments = mongoDatabase.find(findDocument);
        for (Document eachDoc : returnDocuments) {
            returnData.setId(eachDoc.get("_id").toString());
            returnData.setName(eachDoc.get("name").toString());

            tokens.setInterface(eachDoc.get("tokenInterface").toString());
            tokens.setAPI(eachDoc.get("tokenAPI").toString());
            returnData.setTokens(tokens);

            contactDetails.setNumber(eachDoc.get("contactNumber").toString());
            contactDetails.setName(eachDoc.get("contactName").toString());
            contactDetails.setEmail(eachDoc.get("email").toString());
            returnData.setContactDetails(contactDetails);
        }

        return returnData;
    }
}
