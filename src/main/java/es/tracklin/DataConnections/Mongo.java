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
        doc.put("username", clientData.getName());
        doc.put("password", clientData.getPassword());
        doc.put("email", clientData.getContactDetails().getEmail());
        doc.put("contactName", clientData.getContactDetails().getName());
        doc.put("contactNumber", clientData.getContactDetails().getNumber());
        doc.put("tokenInterface", clientData.getTokens().getInterface());
        doc.put("tokenAPI", clientData.getTokens().getAPI());
        doc.put("createdDate", new Date());

        String returnId = "";

        ClientData client = getUser(clientData.getName(), clientData.getPassword());
        if (client.getId().equals("")) {
            mongoDatabase.insertOne(doc);
            returnId = doc.get("_id").toString();
        } else {
            Document updateDoc = getUserDoc(client.getName(), client.getPassword());
            mongoDatabase.updateOne(updateDoc, doc);
            returnId = updateDoc.get("_id").toString();
        }

        return returnId;
    }

    public ClientData getUser(String username, String password) {
        ClientData returnData = new ClientData();
        ClientData.Tokens tokens = returnData.new Tokens();
        ClientData.ContactDetails contactDetails = returnData.new ContactDetails();

        Document eachDoc = getUserDoc(username, password);
        if (eachDoc.size() != 0) {
            returnData.setId(eachDoc.get("_id").toString());
            returnData.setName(eachDoc.get("username").toString());
            returnData.setPassword(eachDoc.get("password").toString());

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

    private Document getUserDoc(String username, String password) {
        Document findDocument = new Document();
        findDocument.put("name", username);
        findDocument.put("password", password);

        Document returnDoc = new Document();
        MongoCollection<Document> mongoDatabase = connectMongo("user");
        FindIterable<Document> returnDocuments = mongoDatabase.find(findDocument);
        for (Document eachDoc : returnDocuments) {
            returnDoc = eachDoc;
        }

        return returnDoc;
    }

    public void deleteUser(String username, String password) {
        MongoCollection<Document> mongoDatabase = connectMongo("user");

        Document findDoc = getUserDoc(username, password);
        mongoDatabase.deleteOne(findDoc);
    }
}
